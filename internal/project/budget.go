package project

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"yuno/internal/store"
)

// Project budgets (DESIGN 5.19): a UTC-day cap and a UTC-month cap, a refusal at the
// cap when a run is queued, and the alerts at 80 % and at the cap. Money is micro-USD
// here; the API shows USD.

// The two windows, as the alert ledger and the 429 body name them.
const (
	WindowDay   = store.BudgetWindowDay
	WindowMonth = store.BudgetWindowMonth
)

// WarningPercent is the share of a cap at which the warning alert is sent (rule 5).
const WarningPercent = 80

// Window is one budget window: the UTC calendar day or month, end exclusive.
type Window struct {
	Kind  string // WindowDay | WindowMonth
	Start time.Time
	End   time.Time
}

// Windows returns the UTC day and the UTC month that contain now (DESIGN 5.19 units).
func Windows(now time.Time) (day, month Window) {
	ds, de := store.DayBounds(now)
	ms, me := store.MonthBounds(now)
	return Window{Kind: WindowDay, Start: ds, End: de}, Window{Kind: WindowMonth, Start: ms, End: me}
}

// adjective is how the texts name the window: daily or monthly.
func (w Window) adjective() string {
	if w.Kind == WindowMonth {
		return "monthly"
	}
	return "daily"
}

// BudgetError is the refusal of a trigger whose project has spent its cap (DESIGN 5.19
// rule 3). Window is WindowDay or WindowMonth.
type BudgetError struct {
	ProjectID   string
	ProjectName string
	Window      string
	CapMicros   int64
	SpendMicros int64
	WindowEnd   time.Time
}

func (e *BudgetError) Error() string {
	return fmt.Sprintf("project %s has reached its %s budget ($%s); new runs are refused until %s",
		e.ProjectName, Window{Kind: e.Window}.adjective(), FormatUSD(e.CapMicros), e.WindowEnd.UTC().Format(time.RFC3339))
}

// FormatUSD prints micro-USD as dollars with two decimals; an amount under a cent keeps
// four, so a tiny cap does not read as zero (as the run reports print cost).
func FormatUSD(micros int64) string {
	usd := float64(micros) / 1e6
	if micros > 0 && usd < 0.01 {
		return fmt.Sprintf("%.4f", usd)
	}
	return fmt.Sprintf("%.2f", usd)
}

// CheckBudget refuses new work on a project whose spend in a window is already at or
// over that window's cap (DESIGN 5.19 rule 3): the day cap is checked before the month
// cap, and spend exactly at the cap refuses. It returns a *BudgetError for the first
// cap reached, nil when none is, or the query's error: a failed spend query must fail
// the trigger, never silently allow it. Call it inside the trigger transaction, next to
// the rate check.
func CheckBudget(ctx context.Context, q store.Querier, p *store.Project, now time.Time) error {
	if p == nil {
		return nil
	}
	day, month := Windows(now)
	for _, c := range []struct {
		w   Window
		cap *store.USDMicros
	}{{day, p.BudgetDayUSDMicros}, {month, p.BudgetMonthUSDMicros}} {
		if c.cap == nil {
			continue
		}
		spent, err := store.SumProjectSpend(ctx, q, p.ID, c.w.Start, c.w.End)
		if err != nil {
			return fmt.Errorf("project %s spend: %w", p.ID, err)
		}
		if spent >= int64(*c.cap) {
			return &BudgetError{ProjectID: p.ID, ProjectName: p.Name, Window: c.w.Kind,
				CapMicros: int64(*c.cap), SpendMicros: spent, WindowEnd: c.w.End}
		}
	}
	return nil
}

// Reporter sends a platform message into a conversation (the engine satisfies it):
// deferred says the gateway could not be reached and the caller tries again later.
type Reporter interface {
	SendPlatformMessage(ctx context.Context, runID, conversationID, key, text string) (deferred bool)
}

// Watcher sends the budget alerts of DESIGN 5.19 rules 5 and 6: every Tick it computes
// the spend of each live project with a cap and sends at most one warning (spend at 80 %
// of the cap or more) and one reached alert (spend at the cap or more) per project,
// window and window start. Sent alerts are rows of project_budget_alerts, so a restart
// re-sends nothing.
type Watcher struct {
	Pool     store.Querier
	Tick     time.Duration
	Log      *slog.Logger
	Now      func() time.Time
	Reporter Reporter
}

// DefaultTick is how often the watcher looks when Tick is unset (BUDGET_TICK).
const DefaultTick = time.Minute

func (w *Watcher) now() time.Time {
	if w.Now != nil {
		return w.Now().UTC()
	}
	return time.Now().UTC()
}

func (w *Watcher) log() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Run checks at start and then every Tick until ctx ends.
func (w *Watcher) Run(ctx context.Context) {
	every := w.Tick
	if every <= 0 {
		every = DefaultTick
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		w.TickOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// destination is where an alert goes: the newest conversation of the agent bound to the
// Telegram channel (rule 6). id is "" when there is none.
type destination struct{ id string }

// TickOnce runs one check of every live project with a cap and returns how many alerts
// it sent. An error on one project or window is logged and the tick goes on.
func (w *Watcher) TickOnce(ctx context.Context) (sent int) {
	log := w.log()
	now := w.now()
	projects, err := store.ListProjects(ctx, w.Pool)
	if err != nil {
		if ctx.Err() == nil {
			log.Warn("budget watcher: list projects failed", "error", err)
		}
		return 0
	}
	day, month := Windows(now)
	var dest *destination // resolved at most once per tick, when an alert is due
	for i := range projects {
		p := &projects[i]
		for _, c := range []struct {
			w   Window
			cap *store.USDMicros
		}{{day, p.BudgetDayUSDMicros}, {month, p.BudgetMonthUSDMicros}} {
			if c.cap == nil || ctx.Err() != nil {
				continue
			}
			n, err := w.checkWindow(ctx, p, c.w, int64(*c.cap), now, &dest)
			sent += n
			if err != nil && ctx.Err() == nil {
				log.Warn("budget watcher: project skipped", "project_id", p.ID, "window", c.w.Kind, "error", err)
			}
		}
	}
	return sent
}

// checkWindow sends the alerts one project's window is due for.
func (w *Watcher) checkWindow(ctx context.Context, p *store.Project, win Window, capMicros int64, now time.Time, dest **destination) (sent int, err error) {
	spent, err := store.SumProjectSpend(ctx, w.Pool, p.ID, win.Start, win.End)
	if err != nil {
		return 0, err
	}
	// Integer comparisons: 80 % is spent*100 >= cap*80, with no float rounding.
	due := map[string]bool{
		store.BudgetAlertWarning: spent*100 >= capMicros*WarningPercent,
		store.BudgetAlertReached: spent >= capMicros,
	}
	if !due[store.BudgetAlertWarning] {
		return 0, nil
	}
	already, err := store.SentBudgetAlerts(ctx, w.Pool, p.ID, win.Kind, win.Start)
	if err != nil {
		return 0, err
	}
	for _, level := range []string{store.BudgetAlertWarning, store.BudgetAlertReached} {
		if !due[level] || already[level] {
			continue
		}
		if *dest == nil {
			d, err := w.resolveDestination(ctx)
			if err != nil {
				return sent, err
			}
			*dest = d
		}
		text := alertText(level, p.Name, win, capMicros, spent)
		log := w.log().With("project_id", p.ID, "window", win.Kind, "level", level)
		if (*dest).id == "" {
			log.Info("budget alert has nowhere to go: no Telegram binding or conversation", "text", text)
		} else {
			key := AlertKey(p.ID, win, level)
			if w.Reporter == nil || w.Reporter.SendPlatformMessage(ctx, "", (*dest).id, key, text) {
				// Nothing is recorded: the next tick tries again, and the later level
				// waits so the messages keep their order.
				log.Info("budget alert deferred; tried again at the next tick")
				return sent, nil
			}
			sent++
		}
		if err := store.RecordBudgetAlert(ctx, w.Pool, p.ID, win.Kind, win.Start, level, now); err != nil {
			return sent, err
		}
	}
	return sent, nil
}

// resolveDestination finds the newest conversation of the agent bound to the Telegram
// channel (rule 6); a destination with an empty id says there is none.
func (w *Watcher) resolveDestination(ctx context.Context) (*destination, error) {
	id, _, err := BoundAgentConversation(ctx, w.Pool)
	if err != nil {
		return nil, err
	}
	return &destination{id: id}, nil
}

// BoundAgentConversation is where a message for the operator goes when its subject has no
// conversation of its own: the conversation with the latest message of the agent bound to
// the Telegram channel (DESIGN 5.19 rule 6, 5.24 rule 6). ok is false, with no error, when
// no agent is bound or the bound agent has no conversation yet.
func BoundAgentConversation(ctx context.Context, q store.Querier) (conversationID string, ok bool, err error) {
	b, err := store.GetChannelBinding(ctx, q, store.ChannelTelegram)
	if errors.Is(err, store.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("channel binding: %w", err)
	}
	c, err := store.LatestConversationForAgent(ctx, q, b.AgentID)
	if errors.Is(err, store.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("conversation: %w", err)
	}
	return c.ID, true, nil
}

// AlertKey is the chat.send idempotency key of an alert (rule 6):
// budget-<project id>-<window>-<window start>-<level>, the start in RFC 3339 UTC. The
// gateway dedupes a repeat of it, which is what makes a crash between the send and the
// ledger row harmless.
func AlertKey(projectID string, win Window, level string) string {
	return "budget-" + projectID + "-" + win.Kind + "-" + win.Start.UTC().Format(time.RFC3339) + "-" + level
}

// alertText is the message of rule 5.
func alertText(level, name string, win Window, capMicros, spent int64) string {
	if level == store.BudgetAlertReached {
		return fmt.Sprintf("[platform] Project %s has reached its $%s %s budget; new runs on it are refused until %s.",
			name, FormatUSD(capMicros), win.adjective(), win.End.UTC().Format(time.RFC3339))
	}
	return fmt.Sprintf("[platform] Project %s has used $%s of its $%s %s budget (%d %%).",
		name, FormatUSD(spent), FormatUSD(capMicros), win.adjective(), spent*100/capMicros)
}

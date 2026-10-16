package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"yuno/internal/channels"
	"yuno/internal/obs"
	"yuno/internal/openclaw"
	"yuno/internal/store"
)

// Channels and conversations (DESIGN 5.8, section 6): which platform agent answers
// Telegram, and the conversations mirrored from its OpenClaw sessions.

const (
	maxBindingBody       = 4 << 10
	botLookupTimeout     = 5 * time.Second
	botLearnWindow       = 2 * time.Minute
	defaultConversations = 100
	maxConversations     = 500
	defaultMessages      = 500
	maxMessages          = 1000
)

type telegramBindingView struct {
	AgentID   string    `json:"agent_id"`
	AgentName string    `json:"agent_name"`
	CreatedAt time.Time `json:"created_at"`
}

type telegramView struct {
	Enabled        bool                 `json:"enabled"`
	Mode           string               `json:"mode"`
	BotUsername    string               `json:"bot_username,omitempty"`
	AllowedUserIDs []int64              `json:"allowed_user_ids"`
	Binding        *telegramBindingView `json:"binding"`
}

type channelsView struct {
	Telegram telegramView `json:"telegram"`
}

type bindingInput struct {
	AgentID string `json:"agent_id"`
}

// The bot username comes from the gateway's channels.status probe, which makes the
// Telegram plugin call getMe: seconds on a slow network, and nothing while the gateway
// restarts. GET /api/channels reads only the cache (audit #27), which is filled at api
// start and again at every bind; "" until known.

func (s *Server) telegramOn() bool {
	return s.gateway != nil && s.cfg.TelegramMode != "" && s.cfg.TelegramMode != "off"
}

// botUsername is the cached bot username, "" until known.
func (s *Server) botUsername() string {
	if p := s.botName.Load(); p != nil {
		return *p
	}
	return ""
}

// refreshBotUsername asks the gateway for the bot and caches the answer; it reports
// whether a name is known.
func (s *Server) refreshBotUsername(ctx context.Context) bool {
	if !s.telegramOn() {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, botLookupTimeout)
	defer cancel()
	name, err := openclaw.TelegramBotUsername(cctx, s.gateway, int(botLookupTimeout/time.Millisecond))
	if err != nil || name == "" {
		s.log.Warn("telegram bot username unavailable", "error", err)
		return false
	}
	s.botName.Store(&name)
	return true
}

// learnBotUsername refreshes the cache, retrying with backoff from 1 s doubling to 30 s
// until the gateway answers or ctx ends. The api runs it at start and after every bind
// (the bind restarts the gateway, so the first attempt may find it away).
func (s *Server) learnBotUsername(ctx context.Context) {
	wait := time.Second
	for s.telegramOn() && !s.refreshBotUsername(ctx) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, 30*time.Second)
	}
}

// channelsState renders GET /api/channels.
func (s *Server) channelsState(ctx context.Context) (channelsView, error) {
	ids := s.cfg.TelegramAllowedUserIDs
	if ids == nil {
		ids = []int64{}
	}
	mode := s.cfg.TelegramMode
	if mode == "" {
		mode = "off"
	}
	v := channelsView{Telegram: telegramView{
		Enabled:        mode == "polling" || mode == "webhook",
		Mode:           mode,
		AllowedUserIDs: ids,
	}}
	b, err := store.GetChannelBinding(ctx, s.pool, store.ChannelTelegram)
	switch {
	case err == nil:
		v.Telegram.Binding = &telegramBindingView{AgentID: b.AgentID, AgentName: b.AgentName, CreatedAt: b.CreatedAt}
	case !errors.Is(err, store.ErrNotFound):
		return v, err
	}
	v.Telegram.BotUsername = s.botUsername()
	if b != nil && v.Telegram.BotUsername != "" && b.ExternalID != v.Telegram.BotUsername {
		if err := store.SetChannelBindingExternalID(ctx, s.pool, store.ChannelTelegram, v.Telegram.BotUsername); err != nil {
			s.log.Warn("record bot username failed", "error", err)
		}
	}
	return v, nil
}

func (s *Server) getChannels(w http.ResponseWriter, r *http.Request) {
	v, err := s.channelsState(r.Context())
	if err != nil {
		s.internalError(w, r, "read channels", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// bindTelegram binds an agent to Telegram (DESIGN 5.8 rule 3, T5.8.3): 409 while
// another agent is bound. The binding is stored first and then applied to the gateway;
// a gateway failure is 502 with the binding id, and the api applies it again at the
// next start or reconnect.
func (s *Server) bindTelegram(w http.ResponseWriter, r *http.Request) {
	var in bindingInput
	if !decodeBody(w, r, &in, maxBindingBody) {
		return
	}
	in.AgentID = strings.TrimSpace(in.AgentID)
	if in.AgentID == "" {
		writeValidation(w, "invalid binding", fieldErrors{{Path: "agent_id", Message: "required"}})
		return
	}
	ctx := r.Context()
	a, err := store.GetAgent(ctx, s.pool, in.AgentID)
	if err != nil {
		s.storeError(w, r, "agent", err)
		return
	}
	if cur, err := store.GetChannelBinding(ctx, s.pool, store.ChannelTelegram); err == nil && cur.AgentID != a.ID {
		writeBound(w, cur)
		return
	}
	if a.OpenClawAgentID == nil || *a.OpenClawAgentID == "" {
		if s.engine == nil {
			writeError(w, http.StatusServiceUnavailable, "engine_unavailable", errEngineUnavailable.Error(), nil)
			return
		}
		if _, err := s.engine.MaterializeAgent(ctx, a.ID); err != nil {
			s.log.Warn("materialise agent before binding failed", "agent_id", a.ID, "error", err)
			writeError(w, http.StatusBadGateway, "gateway_error", "the agent could not be set up on the runtime", nil)
			return
		}
		if a, err = store.GetAgent(ctx, s.pool, a.ID); err != nil {
			s.storeError(w, r, "agent", err)
			return
		}
		if a.OpenClawAgentID == nil || *a.OpenClawAgentID == "" {
			writeError(w, http.StatusBadGateway, "gateway_error", "the agent could not be set up on the runtime", nil)
			return
		}
	}
	b, err := store.BindChannel(ctx, s.pool, store.ChannelTelegram, a.ID)
	if errors.Is(err, store.ErrChannelBound) {
		if cur, gerr := store.GetChannelBinding(ctx, s.pool, store.ChannelTelegram); gerr == nil {
			writeBound(w, cur)
			return
		}
	}
	if err != nil {
		s.internalError(w, r, "bind telegram", err)
		return
	}
	if !s.applyTelegram(w, r, *a.OpenClawAgentID, "yuno: telegram bound to "+a.Name, b.ID) {
		return
	}
	// The bound agent answers people from its workspace, MEMORY.md included.
	s.renderMemory(ctx, a)
	go func() { // the bot username, for the Channels page
		lctx, cancel := context.WithTimeout(context.Background(), botLearnWindow)
		defer cancel()
		s.learnBotUsername(lctx)
	}()
	s.log.Info("telegram bound", "agent_id", a.ID, "openclaw_agent_id", *a.OpenClawAgentID)
	s.writeChannels(w, r)
}

// unbindTelegram removes the binding and closes the channel to direct messages (D-025).
func (s *Server) unbindTelegram(w http.ResponseWriter, r *http.Request) {
	removed, err := store.UnbindChannel(r.Context(), s.pool, store.ChannelTelegram)
	if err != nil {
		s.internalError(w, r, "unbind telegram", err)
		return
	}
	if !s.applyTelegram(w, r, "", "yuno: telegram unbound", "") {
		return
	}
	if removed {
		s.log.Info("telegram unbound")
	}
	s.writeChannels(w, r)
}

func writeBound(w http.ResponseWriter, cur *store.ChannelBinding) {
	writeError(w, http.StatusConflict, "channel_bound",
		"Telegram is answered by "+cur.AgentName+"; unbind it first",
		map[string]string{"agent_id": cur.AgentID, "agent_name": cur.AgentName})
}

// applyTelegram pushes the binding to the gateway; it answers 502 itself on failure.
func (s *Server) applyTelegram(w http.ResponseWriter, r *http.Request, openclawAgentID, reason, bindingID string) bool {
	// A client that goes away must not leave a patch applied without its restart.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), channels.ApplyTimeout)
	defer cancel()
	res, err := channels.ApplyTelegram(ctx, s.gateway, openclawAgentID, reason)
	if err != nil {
		s.log.Warn("apply telegram binding on the gateway failed", "request_id", obs.RequestID(r.Context()), "error", err)
		var details any
		if bindingID != "" {
			details = map[string]string{"binding_id": bindingID}
		}
		writeError(w, http.StatusBadGateway, "gateway_error",
			"saved, but the runtime did not take the change; it is applied again when the api reconnects", details)
		return false
	}
	if res.Changed {
		s.log.Info("telegram binding applied on the gateway", "openclaw_agent_id", openclawAgentID, "restart", res.Restart)
	}
	return true
}

func (s *Server) writeChannels(w http.ResponseWriter, r *http.Request) {
	v, err := s.channelsState(r.Context())
	if err != nil {
		s.internalError(w, r, "read channels", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// listConversations lists conversations, most recently active first.
func (s *Server) listConversations(w http.ResponseWriter, r *http.Request) {
	limit, ok := conversationsLimit(w, r, defaultConversations, maxConversations)
	if !ok {
		return
	}
	out, err := store.ListConversations(r.Context(), s.pool, limit)
	if err != nil {
		s.internalError(w, r, "list conversations", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// listConversationMessages returns a conversation's newest messages, oldest first.
func (s *Server) listConversationMessages(w http.ResponseWriter, r *http.Request) {
	limit, ok := conversationsLimit(w, r, defaultMessages, maxMessages)
	if !ok {
		return
	}
	ctx := r.Context()
	c, err := store.GetConversation(ctx, s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "conversation", err)
		return
	}
	out, err := store.ListConversationMessages(ctx, s.pool, c.ID, limit, time.Now())
	if err != nil {
		s.internalError(w, r, "list conversation messages", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func conversationsLimit(w http.ResponseWriter, r *http.Request, def, max int) (int, bool) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > max {
		writeValidation(w, "invalid query", fieldErrors{{Path: "limit", Message: "must be an integer from 1 to " + strconv.Itoa(max)}})
		return 0, false
	}
	return n, true
}

// unbindAgent closes Telegram when agentID, just deleted, was the bound agent (D-025):
// a deleted agent must not keep answering until the next re-assert. Failures are
// logged; the keeper closes the channel at the next start or reconnect.
func (s *Server) unbindAgent(ctx context.Context, agentID string) {
	removed, err := store.UnbindAgent(ctx, s.pool, agentID)
	if err != nil || !removed {
		if err != nil {
			s.log.Warn("unbind deleted agent failed", "agent_id", agentID, "error", err)
		}
		return
	}
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), channels.ApplyTimeout)
	defer cancel()
	if _, err := channels.ApplyTelegram(actx, s.gateway, "", "yuno: bound agent deleted"); err != nil {
		s.log.Warn("close telegram after deleting the bound agent failed", "agent_id", agentID, "error", err)
		return
	}
	s.log.Info("telegram unbound: the bound agent was deleted", "agent_id", agentID)
}

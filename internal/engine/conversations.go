package engine

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"yuno/internal/channels"
	"yuno/internal/guardrails"
	"yuno/internal/obs"
	"yuno/internal/openclaw"
	"yuno/internal/store"
)

// Conversations (DESIGN 5.8 rules 4, 5, 7). A Telegram conversation is the bound
// agent's own OpenClaw session, not a workflow run: OpenClaw runs the turn and sends
// the reply. The engine follows the session roster, subscribes to each Telegram
// session of the bound agent, and mirrors its transcript into conversations /
// conversation_messages and its exec approvals into approvals (no run).

const (
	convHistoryLimit = 200
	convResyncEvery  = 5 * time.Second // retry of a roster sync or a catch-up that failed
	convRetryMax     = time.Minute     // longest wait between catch-up retries of a session
	convListMinGap   = 2 * time.Second // roster-wide events re-list at most this often
)

// EventConversationMessage is the SSE event for a mirrored message.
const EventConversationMessage = "conversation.message"

type convSession struct {
	conv     *store.Conversation
	ocID     string
	seen     map[string]bool
	approved map[string]*store.Approval // exec approvals by gateway id
}

type convMirror struct {
	e        *Engine
	gw       openclaw.Client
	log      *slog.Logger
	sessions map[string]*convSession // subscribed on the current connection, by key
	unsynced map[string]*convRetry   // followed sessions whose catch-up failed, by key
	warned   map[string]bool         // sessions that reached an agent not bound to the channel
	lastList time.Time
	synced   bool
}

// convRetry is when a session's failed catch-up is tried again, with backoff.
type convRetry struct {
	next  time.Time
	delay time.Duration
}

// mirrorConversations runs the mirror until ctx ends.
func (e *Engine) mirrorConversations(ctx context.Context) {
	gw := e.deps.Gateway
	if gw == nil || e.deps.Pool == nil {
		return
	}
	events, unsub := gw.Subscribe(1024)
	defer unsub()
	m := &convMirror{e: e, gw: gw, log: e.log.With("component", "conversations"),
		sessions: map[string]*convSession{}, unsynced: map[string]*convRetry{}, warned: map[string]bool{}}
	m.sync(ctx)
	retry := time.NewTicker(e.convRetry)
	defer retry.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			m.handle(ctx, ev)
		case <-retry.C:
			if !m.synced {
				m.sync(ctx)
			}
			m.retryUnsynced(ctx)
		}
	}
}

func (m *convMirror) handle(ctx context.Context, ev openclaw.Event) {
	switch ev.Name {
	case openclaw.EventReconnected:
		// Subscriptions belong to the connection that made them; following again
		// catches every session up.
		m.sessions = map[string]*convSession{}
		m.unsynced = map[string]*convRetry{}
		m.sync(ctx)
	case openclaw.EventGap:
		for key, s := range m.sessions {
			m.catchUp(ctx, key, s)
		}
	case openclaw.EventSessionsChanged:
		var sc openclaw.SessionsChangedEvent
		if json.Unmarshal(ev.Payload, &sc) != nil {
			return
		}
		row := sc.Row()
		if row.Key == "" {
			// A roster-wide change without the session: list again, not too often.
			if time.Since(m.lastList) >= convListMinGap {
				m.sync(ctx)
			}
			return
		}
		m.follow(ctx, row, true)
	case "session.message":
		var sm openclaw.SessionMessage
		if json.Unmarshal(ev.Payload, &sm) != nil {
			return
		}
		if s := m.sessions[sm.SessionKey]; s != nil {
			m.mirror(ctx, s, sm.Message, sm.MessageID, time.Time{})
		}
	case "session.approval":
		var sa openclaw.SessionApprovalEvent
		if json.Unmarshal(ev.Payload, &sa) != nil {
			return
		}
		if s := m.sessions[sa.SessionKey]; s != nil {
			m.onApproval(ctx, s, sa)
		}
	}
}

// sync subscribes to the roster and follows every existing session. While the
// gateway is away it waits for the reconnect, which syncs again.
func (m *convMirror) sync(ctx context.Context) {
	m.lastList = time.Now()
	if _, up := m.gw.Info(); !up {
		m.synced = false
		return
	}
	cctx, cancel := context.WithTimeout(ctx, gatewayRPCTimeout)
	defer cancel()
	if err := openclaw.SessionsSubscribeRoster(cctx, m.gw); err != nil {
		m.synced = false
		if ctx.Err() == nil {
			m.log.Warn("subscribe to the session roster failed", "error", err)
		}
		return
	}
	rows, err := openclaw.SessionsList(cctx, m.gw)
	if err != nil {
		m.synced = false
		if ctx.Err() == nil {
			m.log.Warn("list sessions failed", "error", err)
		}
		return
	}
	m.synced = true
	for _, row := range rows {
		m.follow(ctx, row, false)
	}
}

// follow subscribes to a Telegram session of the bound agent the first time it is seen;
// news of a followed session whose catch-up failed tries the catch-up again. A Telegram
// session of any other agent means a message reached an agent the platform did not bind
// (D-025): it is logged once, when it happens (live).
func (m *convMirror) follow(ctx context.Context, row openclaw.SessionRow, live bool) {
	if s, ok := m.sessions[row.Key]; ok {
		if m.unsynced[row.Key] != nil {
			m.catchUp(ctx, row.Key, s)
		}
		return
	}
	k, isChannel := openclaw.ParseChannelSessionKey(row.Key)
	fromTelegram := (isChannel && k.Channel == store.ChannelTelegram) || row.FromChannel(store.ChannelTelegram)
	if !fromTelegram {
		return
	}
	bound, _, err := channels.BoundOpenClawAgent(ctx, m.e.deps.Pool)
	if err != nil {
		m.log.Warn("read the telegram binding failed", "error", err)
		return
	}
	agentID := k.AgentID
	if !isChannel {
		agentID = row.AgentID
	}
	if !isChannel || k.Channel != store.ChannelTelegram || bound == "" || agentID != bound {
		if live && !m.warned[row.Key] {
			m.warned[row.Key] = true
			m.log.Warn("a telegram message reached an agent that is not bound to the channel",
				"session_key", row.Key, "openclaw_agent_id", agentID, "bound_openclaw_agent_id", bound)
		}
		return
	}
	conv := &store.Conversation{Channel: store.ChannelTelegram, ExternalChatID: k.PeerID, SessionKey: row.Key,
		DisplayName: row.DisplayName}
	if conv.DisplayName == "" && row.Origin != nil {
		conv.DisplayName = row.Origin.Label
	}
	if a, err := store.GetAgentByOpenClawID(ctx, m.e.deps.Pool, k.AgentID); err == nil {
		conv.AgentID = a.ID
	}
	saved, err := store.UpsertConversation(ctx, m.e.deps.Pool, conv)
	if err != nil {
		m.log.Warn("store conversation failed", "session_key", row.Key, "error", err)
		return
	}
	cctx, cancel := context.WithTimeout(ctx, gatewayRPCTimeout)
	_, err = openclaw.SessionsSubscribe(cctx, m.gw, row.Key, k.AgentID, true)
	cancel()
	if err != nil {
		m.log.Warn("subscribe to the conversation failed", "session_key", row.Key, "error", err)
		return
	}
	s := &convSession{conv: saved, ocID: k.AgentID, seen: map[string]bool{}, approved: map[string]*store.Approval{}}
	m.sessions[row.Key] = s
	m.log.Info("following conversation", "conversation_id", saved.ID, "session_key", row.Key)
	m.catchUp(ctx, row.Key, s)
}

// catchUp mirrors the session's history: messages sent while the api was away or the
// event buffer overflowed. The store dedupes by message id; entries the conversation's
// TTL has expired are not stored again (DESIGN 5.10 rule 6). A failure leaves the
// session unsynced, and the retry ticker or the session's next roster event tries again.
func (m *convMirror) catchUp(ctx context.Context, key string, s *convSession) {
	days, err := store.ConversationTTLDays(ctx, m.e.deps.Pool, s.conv.ID)
	if err != nil {
		m.log.Warn("read conversation retention failed", "session_key", key, "error", err)
		m.markUnsynced(key)
		return
	}
	cctx, cancel := context.WithTimeout(ctx, gatewayRPCTimeout)
	h, err := openclaw.ChatHistory(cctx, m.gw, key, s.ocID, convHistoryLimit)
	cancel()
	if err != nil {
		m.log.Warn("read conversation history failed", "session_key", key, "error", err)
		m.markUnsynced(key)
		return
	}
	if m.unsynced[key] != nil {
		delete(m.unsynced, key)
		m.log.Info("conversation history caught up after a failure", "conversation_id", s.conv.ID, "session_key", key)
	}
	notBefore := m.e.clock.Now().Add(-time.Duration(days) * 24 * time.Hour)
	for _, raw := range h.Messages {
		m.mirror(ctx, s, raw, "", notBefore)
	}
}

// markUnsynced schedules another catch-up of the session, backing off from the retry
// tick to convRetryMax.
func (m *convMirror) markUnsynced(key string) {
	r := m.unsynced[key]
	if r == nil {
		r = &convRetry{delay: m.e.convRetry}
		m.unsynced[key] = r
	} else {
		r.delay = min(2*r.delay, convRetryMax)
	}
	r.next = time.Now().Add(r.delay)
}

// retryUnsynced catches up the sessions whose retry is due.
func (m *convMirror) retryUnsynced(ctx context.Context) {
	now := time.Now()
	for key, r := range m.unsynced {
		s := m.sessions[key]
		switch {
		case s == nil:
			delete(m.unsynced, key)
		case !now.Before(r.next):
			m.catchUp(ctx, key, s)
		}
	}
}

// mirror stores one transcript entry: the person's message (in) or the agent's
// answer (out), text only, unless it is older than notBefore. Tool calls, tool results
// and reasoning are not part of the conversation (DESIGN 5.14 rule 11).
func (m *convMirror) mirror(ctx context.Context, s *convSession, raw json.RawMessage, messageID string, notBefore time.Time) {
	var h openclaw.HistoryEntry
	if json.Unmarshal(raw, &h) != nil {
		return
	}
	var dir string
	switch h.Role {
	case "user":
		dir = store.DirectionIn
	case "assistant":
		dir = store.DirectionOut
	default:
		return
	}
	id := messageID
	if id == "" {
		id = h.Meta().ID
	}
	if id == "" {
		id = h.Role + "@" + strconv.FormatInt(h.Timestamp, 10)
	}
	if s.seen[id] {
		return
	}
	// An assistant turn can arrive first with only tool calls and later, under the same
	// message id, with its text; the id is marked seen only once there is text to store.
	text := strings.TrimSpace(h.Text())
	if text == "" {
		return
	}
	s.seen[id] = true
	msg := &store.ConversationMessage{ConversationID: s.conv.ID, Direction: dir, ExternalMessageID: id,
		Text: obs.Redact(text), CreatedAt: m.e.clock.Now()}
	if h.Timestamp > 0 {
		msg.CreatedAt = msTime(h.Timestamp)
	}
	if msg.CreatedAt.Before(notBefore) {
		return
	}
	if dir == store.DirectionOut && h.Usage != nil {
		msg.Usage, _ = json.Marshal(h.Usage)
	}
	if dir == store.DirectionIn {
		msg.RunID = m.reportedRun(ctx, h.IdempotencyKey)
	}
	inserted, err := store.InsertConversationMessage(ctx, m.e.deps.Pool, msg)
	if err != nil {
		m.log.Warn("store conversation message failed", "conversation_id", s.conv.ID, "error", err)
		delete(s.seen, id)
		return
	}
	if !inserted {
		return
	}
	if c, err := store.GetConversation(ctx, m.e.deps.Pool, s.conv.ID); err == nil {
		s.conv = c
	}
	m.e.emit(ctx, "", EventConversationMessage, map[string]any{"conversation": s.conv, "message": msg})
}

// reportedRun returns the run a platform report was about when the user entry is one
// the engine injected (DESIGN 5.15 rule 11): its idempotency key is the report's key
// plus ":user" [probed]. Entries the person sent, and reports about a run that no longer
// exists, link to no run.
func (m *convMirror) reportedRun(ctx context.Context, idempotencyKey string) *string {
	id, ok := reportRunID(idempotencyKey)
	if !ok {
		return nil
	}
	if _, err := store.GetRun(ctx, m.e.deps.Pool, id); err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			m.log.Warn("read the reported run failed", "run_id", id, "error", err)
		}
		return nil
	}
	return &id
}

// onApproval mirrors an exec approval raised by a conversation turn (DESIGN 5.8 rule
// 5). OpenClaw delivers it to the approvers' DMs; the platform lists it in the inbox,
// with the conversation instead of a run, and decides it like any exec approval.
func (m *convMirror) onApproval(ctx context.Context, s *convSession, sa openclaw.SessionApprovalEvent) {
	ap := sa.Approval
	if ap.ID == "" || ap.Presentation.Kind == "plugin" || strings.HasPrefix(ap.ID, "plugin:") {
		return
	}
	pool := m.e.deps.Pool
	a := s.approved[ap.ID]
	if a == nil {
		if cur, err := store.GetConversationApprovalByExternalID(ctx, pool, ap.ID); err == nil {
			a = cur
		}
	}
	if a == nil {
		requested := m.e.clock.Now()
		if ap.CreatedAtMs > 0 {
			requested = msTime(ap.CreatedAtMs)
		}
		expires := requested.Add(30 * time.Minute)
		if ap.ExpiresAtMs > 0 {
			expires = msTime(ap.ExpiresAtMs)
		}
		id := ap.ID
		a = &store.Approval{Kind: store.ApprovalKindExec, Message: obs.Redact(ap.Presentation.CommandText),
			Status: store.ApprovalPending, ExternalID: &id, RequestedAt: requested, ExpiresAt: expires}
		if err := store.InsertConversationApproval(ctx, pool, a, s.conv.ID); err != nil {
			m.log.Warn("store conversation approval failed", "conversation_id", s.conv.ID, "error", err)
			return
		}
		m.e.emit(ctx, "", "approval.requested", map[string]any{"approval": a, "conversation_id": s.conv.ID})
		m.log.Info("exec approval raised in a conversation", "conversation_id", s.conv.ID, "approval_id", a.ID)
		s.approved[ap.ID] = a
		if sa.Phase != "terminal" && m.denyBlocked(ctx, s, a, ap.Presentation.CommandText) {
			return
		}
	}
	s.approved[ap.ID] = a
	if sa.Phase != "terminal" || a.Status != store.ApprovalPending {
		return
	}
	status := approvalStatus[ap.Status]
	if status == "" {
		status = store.ApprovalRejected
		if strings.HasPrefix(ap.Decision, "allow") {
			status = store.ApprovalApproved
		}
	}
	reason := ap.Reason
	if reason == "" {
		reason = ap.Decision
	}
	at := m.e.clock.Now()
	if ap.ResolvedAtMs > 0 {
		at = msTime(ap.ResolvedAtMs)
	}
	decided, err := store.DecideConversationApproval(ctx, pool, a.ID, status, "openclaw", reason, at)
	if errors.Is(err, store.ErrNotFound) {
		return // decided on the platform first
	}
	if err != nil {
		m.log.Warn("decide conversation approval failed", "approval_id", a.ID, "error", err)
		return
	}
	s.approved[ap.ID] = decided
	m.e.emit(ctx, "", "approval.decided", map[string]any{"approval": decided, "conversation_id": s.conv.ID})
}

// denyBlocked denies a command one of the conversation agent's exec patterns blocks
// (DESIGN 5.11 rule 4), on the gateway and in the row, and reports whether it did.
func (m *convMirror) denyBlocked(ctx context.Context, s *convSession, a *store.Approval, command string) bool {
	if s.conv.AgentID == "" {
		return false
	}
	agent, err := store.GetAgent(ctx, m.e.deps.Pool, s.conv.AgentID)
	if err != nil {
		return false
	}
	p, blocked := guardrails.MatchCommand(agent.Guardrails, command)
	if !blocked {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, gatewayRPCTimeout)
	err = openclaw.ExecApprovalResolve(cctx, m.gw, *a.ExternalID, "deny")
	cancel()
	if err != nil {
		m.log.Warn("deny blocked command on the gateway failed", "approval_id", a.ID, "error", err)
	}
	decided, err := store.DecideConversationApproval(ctx, m.e.deps.Pool, a.ID, store.ApprovalRejected, "guardrail",
		guardrails.BlockedMessage(p), m.e.clock.Now())
	if err != nil {
		return false
	}
	s.approved[*a.ExternalID] = decided
	m.e.emit(ctx, "", "approval.decided", map[string]any{"approval": decided, "conversation_id": s.conv.ID})
	return true
}

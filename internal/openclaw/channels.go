package openclaw

import (
	"context"
	"encoding/json"
	"strings"
)

// Gateway facts behind the Telegram channel (DESIGN 5.8), probed on 2026.10.3 with
// scripts/openclaw-probe-telegram.mjs.

// GatewayRestartResult is the payload of gateway.restart.request [probed]: status
// "scheduled" (now), "deferred" (after the turns in flight) or "coalesced" (one is
// already pending). In a container the gateway restarts in place: same process, same
// container, every client connection closed with 1012 and refused with "gateway
// starting; retry shortly" (retryable) for a few seconds.
type GatewayRestartResult struct {
	OK        bool   `json:"ok"`
	Status    string `json:"status"`
	Preflight struct {
		Safe    bool   `json:"safe"`
		Summary string `json:"summary"`
	} `json:"preflight"`
}

// GatewayRestart asks the gateway for its safe restart, which waits for active work.
func GatewayRestart(ctx context.Context, c Client, reason string) (GatewayRestartResult, error) {
	var out GatewayRestartResult
	err := CallAs(ctx, c, "gateway.restart.request", map[string]any{"reason": reason}, &out)
	return out, err
}

// TelegramBotUsername asks the Telegram channel for its bot through channels.status
// with probe (the plugin calls getMe) [probed]; the platform never handles the token.
// It returns "" when the channel is not configured or the probe failed.
func TelegramBotUsername(ctx context.Context, c Client, timeoutMs int) (string, error) {
	var out struct {
		Channels map[string]struct {
			Probe struct {
				OK  bool `json:"ok"`
				Bot struct {
					Username string `json:"username"`
				} `json:"bot"`
			} `json:"probe"`
		} `json:"channels"`
	}
	err := CallAs(ctx, c, "channels.status", map[string]any{"channel": "telegram", "probe": true, "timeoutMs": timeoutMs}, &out)
	if err != nil {
		return "", err
	}
	return out.Channels["telegram"].Probe.Bot.Username, nil
}

// EventSessionsChanged is the roster event a connection receives after sessions.subscribe.
const EventSessionsChanged = "sessions.changed"

// SessionsSubscribeRoster subscribes this connection to sessions.changed. Like every
// subscription it is per connection and must be renewed after a reconnect.
func SessionsSubscribeRoster(ctx context.Context, c Client) error {
	_, err := c.Call(ctx, "sessions.subscribe", map[string]any{})
	return err
}

// SessionOrigin is where a channel session's first message came from [probed]:
// {provider: "telegram", from: "telegram:<user>", to: "telegram:<user>", chatType:
// "direct", label: "<name> (@handle) id:<user>"}.
type SessionOrigin struct {
	Provider string `json:"provider,omitempty"`
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
	ChatType string `json:"chatType,omitempty"`
	Label    string `json:"label,omitempty"`
}

// SessionRow is the subset of a sessions.list entry (and of the session snapshot in a
// sessions.changed event) the platform reads [probed].
type SessionRow struct {
	Key         string         `json:"key"`
	AgentID     string         `json:"agentId"`
	Channel     string         `json:"channel,omitempty"`
	ChatType    string         `json:"chatType,omitempty"`
	DisplayName string         `json:"displayName,omitempty"`
	CreatedVia  string         `json:"createdVia,omitempty"`
	Origin      *SessionOrigin `json:"origin,omitempty"`
	UpdatedAt   int64          `json:"updatedAt,omitempty"`
}

// SessionsList returns the gateway's sessions.
func SessionsList(ctx context.Context, c Client) ([]SessionRow, error) {
	var out struct {
		Sessions []SessionRow `json:"sessions"`
	}
	err := CallAs(ctx, c, "sessions.list", map[string]any{}, &out)
	return out.Sessions, err
}

// SessionsChangedEvent is the payload of sessions.changed. The gateway sends the key as
// sessionKey and may spread the session row into the payload or nest it under session;
// Row normalises both.
type SessionsChangedEvent struct {
	SessionKey  string         `json:"sessionKey"`
	Key         string         `json:"key"`
	AgentID     string         `json:"agentId"`
	Phase       string         `json:"phase,omitempty"`
	Reason      string         `json:"reason,omitempty"`
	Channel     string         `json:"channel,omitempty"`
	DisplayName string         `json:"displayName,omitempty"`
	CreatedVia  string         `json:"createdVia,omitempty"`
	Origin      *SessionOrigin `json:"origin,omitempty"`
	Session     *SessionRow    `json:"session,omitempty"`
}

// Row returns the session the event is about; Key is empty for roster-wide events.
func (e SessionsChangedEvent) Row() SessionRow {
	r := SessionRow{}
	if e.Session != nil {
		r = *e.Session
	}
	if r.Key == "" {
		r.Key = firstNonEmpty(e.SessionKey, e.Key)
	}
	if r.AgentID == "" {
		r.AgentID = e.AgentID
	}
	if r.Channel == "" {
		r.Channel = e.Channel
	}
	if r.DisplayName == "" {
		r.DisplayName = e.DisplayName
	}
	if r.CreatedVia == "" {
		r.CreatedVia = e.CreatedVia
	}
	if r.Origin == nil {
		r.Origin = e.Origin
	}
	return r
}

// FromChannel reports whether the session was started by a message on channel.
func (r SessionRow) FromChannel(channel string) bool {
	if r.Origin != nil && r.Origin.Provider == channel {
		return true
	}
	return r.Channel == channel
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// ChannelSessionKey is a session key the gateway derives for a channel peer. With a
// binding's session.dmScope "per-channel-peer" a direct chat is
// agent:<agent>:<channel>:direct:<peer id>, the peer id lowercased [bundle
// buildAgentPeerSessionKey; verified against sessions.list once a DM reaches a bound
// agent].
type ChannelSessionKey struct {
	AgentID  string
	Channel  string
	PeerKind string // direct | group | channel
	PeerID   string
}

// ParseChannelSessionKey parses a per-channel-peer session key; ok is false for any
// other shape (node sessions, the default agent's agent:<id>:main).
func ParseChannelSessionKey(key string) (ChannelSessionKey, bool) {
	parts := strings.Split(key, ":")
	if len(parts) != 5 || parts[0] != "agent" || parts[1] == "" || parts[2] == "" || parts[4] == "" {
		return ChannelSessionKey{}, false
	}
	switch parts[3] {
	case "direct", "group", "channel":
	default:
		return ChannelSessionKey{}, false
	}
	return ChannelSessionKey{AgentID: parts[1], Channel: parts[2], PeerKind: parts[3], PeerID: parts[4]}, true
}

// String renders the key back.
func (k ChannelSessionKey) String() string {
	return "agent:" + k.AgentID + ":" + k.Channel + ":" + k.PeerKind + ":" + strings.ToLower(k.PeerID)
}

// SessionMessage is the payload of session.message with the entry kept raw.
type SessionMessage struct {
	SessionKey string          `json:"sessionKey"`
	AgentID    string          `json:"agentId"`
	MessageID  string          `json:"messageId"`
	Message    json.RawMessage `json:"message"`
}

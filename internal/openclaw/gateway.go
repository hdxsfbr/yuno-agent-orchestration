package openclaw

import (
	"context"
	"errors"
	"log/slog"
)

// DialConfig is how the control plane reaches the gateway (DESIGN 5.14 rules 1 and 2).
type DialConfig struct {
	URL     string // ws://127.0.0.1:18789
	Token   string // shared gateway token
	Version string // reported as client.version
}

// Dial connects as client.id "gateway-client", client.mode "backend", role operator,
// scopes read/write/admin/approvals, caps ["approvals", "exec-approvals"], and keeps
// the connection alive with reconnect and backoff. It returns once the first handshake
// succeeded or ctx ends. Compile-time stub; wave 1 replaces this file.
func Dial(ctx context.Context, cfg DialConfig, log *slog.Logger) (Client, error) {
	return nil, errors.New("openclaw: gateway client not implemented yet")
}

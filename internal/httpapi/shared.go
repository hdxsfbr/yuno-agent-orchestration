package httpapi

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"yuno/internal/store"
)

// Shared is the part of the API the seed reuses (DESIGN 5.15 rules 13 and 14): the
// template instantiation behind POST /api/workflow-templates/{key}/instantiate and the
// bind code behind PUT /api/channels/telegram/binding, without routes, platform tools or
// an event recorder.
type Shared struct{ s *Server }

// NewShared builds Shared from the server's dependencies; Engine and Gateway are only
// needed to bind.
func NewShared(d Deps) *Shared {
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	return &Shared{s: &Server{cfg: d.Cfg, log: d.Log, pool: d.Pool, engine: d.Engine, gateway: d.Gateway}}
}

// InstantiateTemplate creates a workflow from the template with key, named name (the
// template's name when empty), exactly as the API's "Create copy" does. Problems with
// the template, such as an agent it names that does not exist, are one error listing
// them.
func (sh *Shared) InstantiateTemplate(ctx context.Context, key, name string) (*store.Workflow, error) {
	wf, fe, err := sh.s.instantiate(ctx, key, name)
	if err != nil {
		return nil, err
	}
	if len(fe) > 0 {
		msgs := make([]string, len(fe))
		for i, f := range fe {
			msgs[i] = f.Path + ": " + f.Message
		}
		return nil, fmt.Errorf("template %s cannot be instantiated: %s", key, strings.Join(msgs, "; "))
	}
	return wf, nil
}

// BindTelegram binds the agent to Telegram as the Channels page does: refused while
// another agent is bound, materialised first when needed, then stored and applied on
// the gateway, which restarts to take it.
func (sh *Shared) BindTelegram(ctx context.Context, agentID string) error {
	_, err := sh.s.bindAgent(ctx, agentID, "")
	return err
}

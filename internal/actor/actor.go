// Package actor names who makes a request to the platform (DESIGN 5.31 rule 1): the
// operator through the UI or the API, an agent through its platform tools, or a caller
// the bearer check refused. Audit rows, idempotency keys and the platform gate read it from
// the request context.
package actor

import "context"

// Kind is the kind of caller.
type Kind string

const (
	Operator  Kind = "operator"
	Agent     Kind = "agent"
	Anonymous Kind = "anonymous"
)

// Ways a request reached the api.
const (
	ViaUI    = "ui"    // the web UI (it sends X-Yuno-Client: web)
	ViaAPI   = "api"   // any other bearer client
	ViaChief = "chief" // an agent's platform_call, dispatched in-process
)

// Actor is who makes a request. ID and Name are the agent's for an agent; OnBehalfOf is
// the person the agent acts for, "telegram:<chat id>", or "" when it is not known.
type Actor struct {
	Kind       Kind   `json:"kind"`
	ID         string `json:"id,omitempty"`
	Name       string `json:"name,omitempty"`
	Via        string `json:"via"`
	OnBehalfOf string `json:"on_behalf_of,omitempty"`
}

type ctxKey struct{}

// With returns ctx carrying a. Only code in this process can set it: an in-process
// dispatch sets an agent actor, and the api middleware sets the operator or anonymous one
// for a request from the network.
func With(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, ctxKey{}, a)
}

// From returns the actor ctx carries.
func From(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(ctxKey{}).(Actor)
	return a, ok
}

// Note is what an inner layer tells the audit row of its request (DESIGN 5.31 rule 1): the
// platform gate's approval and evidence, or the outcome it decided ("denied",
// "pending_approval"). The audit middleware puts an empty one in the context; nil outside
// a request.
type Note struct {
	Outcome    string
	ErrorCode  string
	ApprovalID string
	Evidence   map[string]any
}

type noteKey struct{}

// WithNote returns ctx carrying a fresh note, and the note.
func WithNote(ctx context.Context) (context.Context, *Note) {
	n := &Note{}
	return context.WithValue(ctx, noteKey{}, n), n
}

// NoteFrom returns the note of ctx's request, nil when it has none.
func NoteFrom(ctx context.Context) *Note {
	n, _ := ctx.Value(noteKey{}).(*Note)
	return n
}

// Label is the actor as one string for logs and decided_by columns: "operator:ui",
// "agent:<name>" (with " for telegram:<id>" when known), "anonymous".
func (a Actor) Label() string {
	switch a.Kind {
	case Agent:
		s := "agent:" + a.Name
		if a.OnBehalfOf != "" {
			s += " for " + a.OnBehalfOf
		}
		return s
	case Operator:
		return "operator:" + a.Via
	}
	return string(a.Kind)
}

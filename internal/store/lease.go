package store

import (
	"context"
	"errors"
	"sync/atomic"
)

// Worker leases (DESIGN 5.33 rules 7 to 9). runs.lease_epoch goes up with every claim and every
// move to queued; the worker keeps the epoch it claimed with in a Lease, which travels in the
// context of everything it does for the run. A write the rules fence carries the epoch in
// its own statement (AND lease_epoch = $epoch, through the run for node and branch rows), so
// the check and the write are one step; a fenced write that touches nothing because the
// epoch moved on returns ErrLeaseLost and marks the lease lost, which the worker reads to
// stop itself (rule 9). A context without a lease, or with another run's, writes unfenced:
// the api, the sweepers and the decisions keep writing as they always did.

// ErrLeaseLost is a fenced write refused because the run's lease epoch is no longer the
// worker's: another worker claimed the run, or it was re-queued (rule 9).
var ErrLeaseLost = errors.New("worker lost its lease on the run")

// Lease is the epoch a worker claimed a run with.
type Lease struct {
	RunID string
	Epoch int64
	lost  atomic.Bool
}

// NewLease is the lease of a claimed run.
func NewLease(run *Run) *Lease { return &Lease{RunID: run.ID, Epoch: run.LeaseEpoch} }

// Lost reports whether a fenced write found the lease gone.
func (l *Lease) Lost() bool { return l != nil && l.lost.Load() }

// lose marks the lease lost and returns ErrLeaseLost.
func (l *Lease) lose() error {
	l.lost.Store(true)
	return ErrLeaseLost
}

type leaseKey struct{}

// WithLease returns ctx carrying the worker's lease.
func WithLease(ctx context.Context, l *Lease) context.Context {
	return context.WithValue(ctx, leaseKey{}, l)
}

// LeaseOf returns the lease ctx carries, nil when it has none.
func LeaseOf(ctx context.Context) *Lease {
	l, _ := ctx.Value(leaseKey{}).(*Lease)
	return l
}

// fence returns the lease that fences a write to runID: ctx's lease when it is that run's,
// nil otherwise.
func fence(ctx context.Context, runID string) *Lease {
	l := LeaseOf(ctx)
	if l == nil || runID == "" || l.RunID != runID {
		return nil
	}
	return l
}

// fenceArgs are the two bind values of the fence predicates: the leased run's id and the
// epoch, both nil for an unfenced write.
func fenceArgs(l *Lease) (any, any) {
	if l == nil {
		return nil, nil
	}
	return l.RunID, l.Epoch
}

// runFence is the predicate of a fenced runs update; $r and $e are the positions of
// fenceArgs (the run's id is the row's own).
func runFence(e string) string {
	return `(` + e + `::bigint IS NULL OR lease_epoch = ` + e + `::bigint)`
}

// childFence is the predicate of a fenced write to a row of the run (a node visit or a
// branch, whose run id column is col): the leased run still has the epoch. $r and $e are
// the positions of fenceArgs.
func childFence(col, r, e string) string {
	return `(` + r + `::text IS NULL OR ` + col + ` <> ` + r + `::text OR EXISTS (SELECT 1 FROM runs lr WHERE lr.id = ` + r +
		`::text AND lr.lease_epoch = ` + e + `::bigint))`
}

// lostOr tells a fenced write that touched nothing apart: ErrLeaseLost when the run's epoch
// moved on (the lease is marked lost), err otherwise. err is the write's own answer for no
// row (ErrNotFound, or nil for an update that may match nothing).
func lostOr(ctx context.Context, q Querier, l *Lease, err error) error {
	if l == nil {
		return err
	}
	var epoch int64
	if qerr := q.QueryRow(ctx, `SELECT lease_epoch FROM runs WHERE id=$1`, l.RunID).Scan(&epoch); qerr == nil && epoch != l.Epoch {
		return l.lose()
	}
	return err
}

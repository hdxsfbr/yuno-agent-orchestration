package store

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func testAgent(t *testing.T, name string) *Agent {
	t.Helper()
	a := &Agent{Name: name, Model: "claude-opus-5-5", ToolAllowlist: []string{"finish"}}
	mustDo(t, CreateAgent(context.Background(), testPool, a))
	return a
}

// DESIGN 5.8 rule 3: two binds of different agents racing for the channel: exactly one
// wins, the other gets ErrChannelBound, and the winner's binding is the one stored.
// Rounds with jitter of a few statement round trips vary which statement of one bind
// lands between those of the other.
func TestBindChannelConcurrent(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	agents := []*Agent{testAgent(t, "First"), testAgent(t, "Second")}
	for round := range 80 {
		if _, err := pool.Exec(ctx, `DELETE FROM channel_bindings`); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, len(agents))
		for i, a := range agents {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				time.Sleep(time.Duration(rand.IntN(3000)) * time.Microsecond)
				_, errs[i] = BindChannel(ctx, pool, ChannelTelegram, a.ID)
			}()
		}
		close(start)
		wg.Wait()
		winner := -1
		for i, err := range errs {
			switch {
			case err == nil && winner == -1:
				winner = i
			case err == nil:
				t.Fatalf("round %d: both binds succeeded", round)
			case !errors.Is(err, ErrChannelBound):
				t.Fatalf("round %d: bind %d: %v", round, i, err)
			}
		}
		if winner == -1 {
			t.Fatalf("round %d: neither bind succeeded: %v", round, errs)
		}
		b, err := GetChannelBinding(ctx, pool, ChannelTelegram)
		if err != nil || b.AgentID != agents[winner].ID {
			t.Fatalf("round %d: stored binding %+v (%v), want the winner %s", round, b, err, agents[winner].Name)
		}
	}
}

// interleaved runs another bind to completion just before statement number at of the
// bind it wraps, so a test can place the competitor between any two statements; past
// the last statement it runs after the bind (finish).
type interleaved struct {
	Querier
	at, n int
	ran   bool
	other func()
}

func (q *interleaved) step() {
	if q.n == q.at && !q.ran {
		q.ran = true
		q.other()
	}
	q.n++
}

func (q *interleaved) finish() {
	if !q.ran {
		q.ran = true
		q.other()
	}
}

func (q *interleaved) Exec(ctx context.Context, sql string, args ...any) (pgconnCommandTag, error) {
	q.step()
	return q.Querier.Exec(ctx, sql, args...)
}

func (q *interleaved) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	q.step()
	return q.Querier.Query(ctx, sql, args...)
}

func (q *interleaved) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	q.step()
	return q.Querier.QueryRow(ctx, sql, args...)
}

// The same race, deterministically: the competing bind completes before each statement
// of the other in turn. Whatever the position, one bind wins and its binding stays.
func TestBindChannelInterleaved(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	first, second := testAgent(t, "First"), testAgent(t, "Second")
	for at := range 5 {
		if _, err := pool.Exec(ctx, `DELETE FROM channel_bindings`); err != nil {
			t.Fatal(err)
		}
		var otherErr error
		q := &interleaved{Querier: pool, at: at, other: func() {
			_, otherErr = BindChannel(ctx, pool, ChannelTelegram, second.ID)
		}}
		_, err := BindChannel(ctx, q, ChannelTelegram, first.ID)
		q.finish()
		winner := first
		switch {
		case err == nil && otherErr == nil:
			t.Fatalf("competitor before statement %d: both binds succeeded", at)
		case err == nil && errors.Is(otherErr, ErrChannelBound):
		case errors.Is(err, ErrChannelBound) && otherErr == nil:
			winner = second
		default:
			t.Fatalf("competitor before statement %d: %v and %v", at, err, otherErr)
		}
		b, gerr := GetChannelBinding(ctx, pool, ChannelTelegram)
		if gerr != nil || b.AgentID != winner.ID {
			t.Fatalf("competitor before statement %d: stored %+v (%v), want %s", at, b, gerr, winner.Name)
		}
	}
}

// The bound agent binding again is a no-op; a deleted agent's binding is replaced.
func TestBindChannelReplacesOnlyDeletedAgents(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	a, b := testAgent(t, "Bound"), testAgent(t, "Other")
	first, err := BindChannel(ctx, pool, ChannelTelegram, a.ID)
	mustDo(t, err)
	if again, err := BindChannel(ctx, pool, ChannelTelegram, a.ID); err != nil || again.ID != first.ID {
		t.Fatalf("rebinding the bound agent: %+v %v", again, err)
	}
	if _, err := BindChannel(ctx, pool, ChannelTelegram, b.ID); !errors.Is(err, ErrChannelBound) {
		t.Fatalf("another agent: %v", err)
	}
	mustDo(t, DeleteAgent(ctx, pool, a.ID))
	if got, err := BindChannel(ctx, pool, ChannelTelegram, b.ID); err != nil || got.AgentID != b.ID {
		t.Fatalf("after the bound agent was deleted: %+v %v", got, err)
	}
}

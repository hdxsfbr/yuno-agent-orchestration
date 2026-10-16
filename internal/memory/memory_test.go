package memory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/store"
)

// testPool is the yuno_test database when DATABASE_URL is set; DB-backed tests skip
// otherwise. Every test works on a fresh agent; the sweep tests only look at their own
// rows because other packages' facts share the table.
var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		ctx := context.Background()
		pool, err := store.Open(ctx, url)
		if err != nil {
			fmt.Fprintln(os.Stderr, "memory tests: open database:", err)
			os.Exit(1)
		}
		if err := store.Migrate(ctx, pool, slog.New(slog.DiscardHandler)); err != nil {
			fmt.Fprintln(os.Stderr, "memory tests: migrate:", err)
			os.Exit(1)
		}
		testPool = pool
	}
	code := m.Run()
	if testPool != nil {
		testPool.Close()
	}
	os.Exit(code)
}

func needDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testPool == nil {
		t.Skip("DATABASE_URL not set")
	}
	return testPool
}

func newAgent(t *testing.T) *store.Agent {
	t.Helper()
	a := &store.Agent{Name: "memory test", Model: "claude-opus-5-5", MemoryPolicy: store.MemoryPolicy{Enabled: true}}
	if err := store.CreateAgent(context.Background(), needDB(t), a); err != nil {
		t.Fatal(err)
	}
	return a
}

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// save stores key=value at now with the policy's expiry.
func save(t *testing.T, a *store.Agent, key, value string, now time.Time, ttl *int, maxFacts int) *store.MemoryFact {
	t.Helper()
	exp := ExpiresAt(a.MemoryPolicy, ttl, now)
	f := &store.MemoryFact{AgentID: a.ID, Key: key, Value: value, ExpiresAt: &exp}
	if err := Save(context.Background(), testPool, f, maxFacts, now); err != nil {
		t.Fatal(err)
	}
	return f
}

func keys(facts []store.MemoryFact) []string {
	out := make([]string, len(facts))
	for i, f := range facts {
		out[i] = f.Key
	}
	return out
}

func intp(v int) *int { return &v }

// Rule 3 units: ttl_days from the call, else the policy, else 30 days; 0 is now.
func TestExpiresAt(t *testing.T) {
	day := 24 * time.Hour
	for _, tc := range []struct {
		name string
		pol  store.MemoryPolicy
		ttl  *int
		want time.Duration
	}{
		{"default", store.MemoryPolicy{}, nil, 30 * day},
		{"policy", store.MemoryPolicy{FactTTLDays: 7}, nil, 7 * day},
		{"call wins", store.MemoryPolicy{FactTTLDays: 7}, intp(2), 2 * day},
		{"zero expires now", store.MemoryPolicy{FactTTLDays: 7}, intp(0), 0},
	} {
		if got := ExpiresAt(tc.pol, tc.ttl, t0); !got.Equal(t0.Add(tc.want)) {
			t.Errorf("%s: %s, want now+%s", tc.name, got, tc.want)
		}
	}
	if MaxFacts(store.MemoryPolicy{}) != 200 || MaxFacts(store.MemoryPolicy{MaxFacts: 5}) != 5 {
		t.Error("max_facts default")
	}
}

// T5.10.1: an expired fact is not returned, before the sweep runs; the sweep then
// deletes it. A fact saved with ttl_days 0 is never returned.
func TestExpiredFactHiddenBeforeSweep(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	a := newAgent(t)
	short := save(t, a, "standup", "Daily at 10:00", t0, intp(1), 0)
	save(t, a, "repo", "github.com/acme/app", t0, nil, 0)
	instant := save(t, a, "scratch", "gone at once", t0, intp(0), 0)

	if got := keys(mustRecall(t, a, "", t0)); !slices.Equal(got, []string{"repo", "standup"}) {
		t.Fatalf("at save time: %v (ttl 0 must never be returned)", got)
	}
	later := t0.Add(25 * time.Hour)
	if got := keys(mustRecall(t, a, "", later)); !slices.Equal(got, []string{"repo"}) {
		t.Fatalf("after expiry, before the sweep: %v", got)
	}
	if got, _ := List(ctx, pool, a.ID, later); len(got) != 1 {
		t.Fatalf("list after expiry: %v", keys(got))
	}
	if !exists(t, short.ID) || !exists(t, instant.ID) {
		t.Fatal("expired rows were deleted before the sweep")
	}
	n, err := SweepExpired(ctx, pool, later)
	if err != nil || n < 2 {
		t.Fatalf("sweep: %d, %v", n, err)
	}
	if exists(t, short.ID) || exists(t, instant.ID) {
		t.Fatal("sweep left expired rows")
	}
	if got := keys(mustRecall(t, a, "", later)); !slices.Equal(got, []string{"repo"}) {
		t.Fatalf("after the sweep: %v", got)
	}
}

// T5.10.2: at max_facts the oldest fact by created_at is evicted. Replacing a key
// keeps its created_at, so it ages from its first save, but it sorts as newest.
func TestCapEvictsOldest(t *testing.T) {
	a := newAgent(t)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	save(t, a, "a", "first", at(1), nil, 3)
	save(t, a, "b", "second", at(2), nil, 3)
	save(t, a, "c", "third", at(3), nil, 3)
	save(t, a, "a", "first, revised", at(4), nil, 3) // replaces: still three facts
	if got := keys(mustRecall(t, a, "", at(4))); !slices.Equal(got, []string{"a", "c", "b"}) {
		t.Fatalf("after replace: %v, want newest first", got)
	}
	save(t, a, "d", "fourth", at(5), nil, 3) // over the cap: a was created first
	if got := keys(mustRecall(t, a, "", at(5))); !slices.Equal(got, []string{"d", "c", "b"}) {
		t.Fatalf("after eviction: %v, want a (oldest by created_at) gone", got)
	}
	// Expired facts do not take a slot.
	save(t, a, "e", "short", at(6), intp(0), 3)
	save(t, a, "f", "fifth", at(7), nil, 3)
	if got := keys(mustRecall(t, a, "", at(7))); !slices.Equal(got, []string{"f", "d", "c"}) {
		t.Fatalf("with an expired fact: %v", got)
	}
}

// Rule 4: every word of the query, in key or value, any case; newest first.
func TestRecallMatching(t *testing.T) {
	a := newAgent(t)
	save(t, a, "deploy cadence", "Releases go out on Thursdays", t0, nil, 0)
	save(t, a, "python version", "The sandbox runs Python 3.12", t0.Add(time.Minute), nil, 0)
	save(t, a, "deploy target", "The local release store", t0.Add(2*time.Minute), nil, 0)
	now := t0.Add(time.Hour)
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"", []string{"deploy target", "python version", "deploy cadence"}},
		{"DEPLOY", []string{"deploy target", "deploy cadence"}},
		{"python sandbox", []string{"python version"}},
		{"deploy thursdays", []string{"deploy cadence"}},
		{"cadence releases", []string{"deploy cadence"}},
		{"version the", []string{"python version"}},
		{"python thursdays", nil},
		{"versionthe", nil},
	} {
		if got := keys(mustRecall(t, a, tc.query, now)); !slices.Equal(got, tc.want) && !(len(got) == 0 && tc.want == nil) {
			t.Errorf("recall %q = %v, want %v", tc.query, got, tc.want)
		}
	}
	got, err := Recall(context.Background(), testPool, a.ID, "", 2, now)
	if err != nil || len(got) != 2 {
		t.Fatalf("limit: %v %v", keys(got), err)
	}
}

func TestSaveValueBound(t *testing.T) {
	a := newAgent(t)
	f := &store.MemoryFact{AgentID: a.ID, Key: "big", Value: strings.Repeat("x", MaxValueBytes+1)}
	if err := Save(context.Background(), needDB(t), f, 0, t0); !errors.Is(err, ErrValueTooLong) {
		t.Fatalf("over 2 KiB: %v", err)
	}
	save(t, a, "fits", strings.Repeat("x", MaxValueBytes), t0, nil, 0)
}

func TestDelete(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	a, other := newAgent(t), newAgent(t)
	f := save(t, a, "k", "v", t0, nil, 0)
	if err := Delete(ctx, pool, other.ID, f.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another agent's fact: %v", err)
	}
	if err := Delete(ctx, pool, a.ID, f.ID); err != nil {
		t.Fatal(err)
	}
	if err := Delete(ctx, pool, a.ID, f.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("twice: %v", err)
	}
}

// Rule 6: the sweeper runs every sweep, keeps going past a failing one, and reports
// what it deleted.
func TestSweeperRunsEverySweep(t *testing.T) {
	pool := needDB(t)
	a := newAgent(t)
	f := save(t, a, "old", "v", t0, intp(1), 0)
	var calls []time.Time
	sw := &Sweeper{Pool: pool, Now: func() time.Time { return t0.Add(48 * time.Hour) }, Also: []SweepFunc{
		func(context.Context, time.Time) (int64, error) { return 0, errors.New("conversations table missing") },
		func(_ context.Context, now time.Time) (int64, error) { calls = append(calls, now); return 3, nil },
	}}
	if n := sw.SweepOnce(context.Background()); n < 4 {
		t.Fatalf("deleted %d, want the expired fact plus the extra sweep's 3", n)
	}
	if len(calls) != 1 || !calls[0].Equal(t0.Add(48*time.Hour)) {
		t.Fatalf("extra sweep calls = %v", calls)
	}
	if exists(t, f.ID) {
		t.Fatal("expired fact survived the sweep")
	}
}

func mustRecall(t *testing.T, a *store.Agent, query string, now time.Time) []store.MemoryFact {
	t.Helper()
	got, err := Recall(context.Background(), testPool, a.ID, query, RecallLimit, now)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func exists(t *testing.T, id string) bool {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM memory_facts WHERE id=$1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

package seed

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"yuno/internal/store"
)

// Seeding twice creates each demo agent once and leaves existing agents alone.
func TestSeedIsIdempotent(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := store.Migrate(ctx, pool, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	created, existing, err := Seed(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(created)+len(existing) != len(Agents) {
		t.Fatalf("first seed: created %v existing %v", created, existing)
	}
	created, existing, err = Seed(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 0 || len(existing) != len(Agents) {
		t.Fatalf("second seed: created %v existing %v", created, existing)
	}
	agents, err := store.ListAgents(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	// The shared test database may hold same-named agents from other tests, so the
	// check is presence, not uniqueness.
	for _, want := range Agents {
		found := false
		for _, a := range agents {
			if a.Name == want.Name {
				found = true
			}
		}
		if !found {
			t.Fatalf("agent %s: not found after seeding", want.Name)
		}
	}
}

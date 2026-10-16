package store

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The store's tests run in their own schema of the test database: they bind the one
// Telegram channel and read every schedule, which other packages' tests running in
// parallel against the same database must not see.
const testSchema = "yuno_store_test"

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		if err := setupDB(url); err != nil {
			fmt.Fprintln(os.Stderr, "store tests: database setup failed:", err)
			os.Exit(1)
		}
	}
	code := m.Run()
	if testPool != nil {
		testPool.Close()
	}
	os.Exit(code)
}

func setupDB(url string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	base, err := Open(ctx, url)
	if err != nil {
		return err
	}
	_, err = base.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS `+testSchema)
	base.Close()
	if err != nil {
		return err
	}
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	if testPool, err = Open(ctx, url+sep+"search_path="+testSchema); err != nil {
		return err
	}
	return Migrate(ctx, testPool, slog.New(slog.DiscardHandler))
}

// needDB skips without a database and empties the tables these tests use otherwise.
func needDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testPool == nil {
		t.Skip("DATABASE_URL not set")
	}
	if _, err := testPool.Exec(context.Background(), `TRUNCATE agents, workflows, runs, schedules, channel_bindings,
		conversations, conversation_messages CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return testPool
}

func mustDo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

package intake

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/store"
)

// The intake's tests run in their own schema of the test database: a pass lists every
// project with intake enabled and every finished issue run it finds, which the rows other
// packages' tests keep in the shared tables must never see.
const testSchema = "yuno_intake_test"

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		if err := setupDB(url); err != nil {
			fmt.Fprintln(os.Stderr, "intake tests: database setup failed:", err)
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
	base, err := store.Open(ctx, url)
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
	if testPool, err = store.Open(ctx, url+sep+"search_path="+testSchema); err != nil {
		return err
	}
	return store.Migrate(ctx, testPool, slog.New(slog.DiscardHandler))
}

// needDB skips without a database and empties the tables these tests use otherwise.
func needDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testPool == nil {
		t.Skip("DATABASE_URL not set")
	}
	if _, err := testPool.Exec(context.Background(), `TRUNCATE merge_requests, projects, runs, workflows CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return testPool
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

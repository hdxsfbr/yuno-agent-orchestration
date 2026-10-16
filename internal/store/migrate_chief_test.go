package store

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Migration 0008 on a database that predates it: copies of the shipped templates made
// with "Create copy" (same name, no template key) are marked as made from them, and
// agents get chat tokens.
func TestMigration0008Upgrade(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	const schema = "yuno_store_upgrade_test"
	base, err := Open(ctx, url)
	mustDo(t, err)
	_, err = base.Exec(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE; CREATE SCHEMA `+schema)
	base.Close()
	mustDo(t, err)
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	pool, err := Open(ctx, url+sep+"search_path="+schema)
	mustDo(t, err)
	defer pool.Close()
	applyBefore(t, ctx, pool, 8)

	def := `{"nodes":[],"edges":[]}`
	_, err = pool.Exec(ctx, `INSERT INTO workflows (id, name, definition, template_key) VALUES
		('t1', 'Dev pipeline', $1, 'dev_pipeline'), ('t2', 'Research pipeline', $1, 'research_pipeline'),
		('copy', 'Dev pipeline', $1, NULL), ('renamed', 'My dev pipeline', $1, NULL), ('gone', 'Research pipeline', $1, NULL)`, def)
	mustDo(t, err)
	_, err = pool.Exec(ctx, `UPDATE workflows SET deleted_at = now() WHERE id = 'gone'`)
	mustDo(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO agents (id, name, model) VALUES ('a-old', 'Old agent', 'claude-opus-5-5')`)
	mustDo(t, err)

	mustDo(t, Migrate(ctx, pool, slog.New(slog.DiscardHandler)))
	from := map[string]*string{}
	rows, err := pool.Query(ctx, `SELECT id, from_template FROM workflows`)
	mustDo(t, err)
	for rows.Next() {
		var id string
		var f *string
		mustDo(t, rows.Scan(&id, &f))
		from[id] = f
	}
	rows.Close()
	for id, want := range map[string]string{"copy": "dev_pipeline", "renamed": "", "gone": "", "t1": "", "t2": ""} {
		got := ""
		if from[id] != nil {
			got = *from[id]
		}
		if got != want {
			t.Errorf("%s: from_template = %q, want %q", id, got, want)
		}
	}
	a, err := GetAgent(ctx, pool, "a-old")
	if err != nil || !strings.HasPrefix(a.ChatToken, "c_") {
		t.Fatalf("old agent = %+v %v", a, err)
	}
}

// applyBefore applies the embedded migrations numbered below version, as an older
// release did.
func applyBefore(t *testing.T, ctx context.Context, pool *pgxpool.Pool, version int) {
	t.Helper()
	_, err := pool.Exec(ctx, `CREATE TABLE schema_migrations (version INT PRIMARY KEY, name TEXT NOT NULL,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`)
	mustDo(t, err)
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	mustDo(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		var v int
		if _, err := fmt.Sscanf(name, "%d_", &v); err != nil || v >= version {
			continue
		}
		sql, err := migrationsFS.ReadFile("migrations/" + name)
		mustDo(t, err)
		_, err = pool.Exec(ctx, string(sql))
		mustDo(t, err)
		_, err = pool.Exec(ctx, `INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, v, name)
		mustDo(t, err)
	}
}

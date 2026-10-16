package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// advisoryLockID serializes migrators across processes (api, eval, migrate).
const advisoryLockID = 727272

// Migrate applies every embedded migrations/NNNN_name.sql not yet recorded in
// schema_migrations, in filename order, each in its own transaction.
func Migrate(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockID); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, advisoryLockID) //nolint:errcheck

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INT PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied := map[int]bool{}
	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var version int
		if _, err := fmt.Sscanf(name, "%d_", &version); err != nil {
			return fmt.Errorf("migration %s: name must start with a number", name)
		}
		if applied[version] {
			continue
		}
		sqlBytes, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
			tx.Rollback(ctx) //nolint:errcheck
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, version, name); err != nil {
			tx.Rollback(ctx) //nolint:errcheck
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		log.Info("migration applied", "name", name)
	}
	// Agents created before migration 0008 get their chat token here, once (DESIGN 5.15
	// rule 1): a one-off backfill in Go, since the token is minted by runtoken.
	filled, err := BackfillChatTokens(ctx, conn)
	if err != nil {
		return fmt.Errorf("backfill chat tokens: %w", err)
	}
	if filled > 0 {
		log.Info("chat tokens backfilled", "agents", filled)
	}
	return nil
}

// MigrateURL opens a pool for url, migrates, and closes it. Used by `yuno migrate`.
func MigrateURL(ctx context.Context, url string, log *slog.Logger) error {
	pool, err := Open(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	return Migrate(ctx, pool, log)
}

package store

import "github.com/jackc/pgx/v5/pgconn"

// pgconnCommandTag is aliased so Querier does not force callers to import pgconn.
type pgconnCommandTag = pgconn.CommandTag

package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	// Pure-Go SQLite driver, registered as "sqlite". Chosen over mattn/go-sqlite3
	// so the Dockerfile can keep building a static binary with CGO_ENABLED=0.
	_ "modernc.org/sqlite"
)

// DB wraps *sql.DB with context-taking helpers whose shape matches what the
// repository layer uses, plus optional SQL echoing (SQL_ECHO=true).
// database/sql has no query hook of its own, unlike pgx's QueryTracer.
type DB struct {
	*sql.DB

	echo bool
}

// NewDB opens the SQLite database and verifies it by pinging.
//
// SQLite allows a single writer, and the worker can have up to
// WorkerBatchSize × HepConcurrentFetches goroutines writing at once. Capping the
// pool at one connection makes database/sql queue them instead of surfacing
// SQLITE_BUSY; at this data volume (tens of thousands of rows) the serialisation
// costs nothing.
func NewDB(ctx context.Context, dsn string, sqlEcho bool) (*DB, error) {
	handle, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	handle.SetMaxOpenConns(1)
	handle.SetMaxIdleConns(1)
	handle.SetConnMaxLifetime(0)

	// Ping to fail fast if the file cannot be opened or created, rather than
	// discovering it later during the first query.
	if err := handle.PingContext(ctx); err != nil {
		handle.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return &DB{DB: handle, echo: sqlEcho}, nil
}

// echoQuery prints the statement and its arguments to stdout when SQL_ECHO is
// enabled. Output intentionally bypasses the structured logger so it stays
// readable during interactive debugging.
func (d *DB) echoQuery(query string, args []any) {
	if !d.echo {
		return
	}
	fmt.Fprintf(os.Stdout, "SQL: %s\nArgs: %v\n", query, args)
}

// Query runs a query that returns rows.
func (d *DB) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	d.echoQuery(query, args)
	return d.DB.QueryContext(ctx, query, args...)
}

// QueryRow runs a query expected to return at most one row.
func (d *DB) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	d.echoQuery(query, args)
	return d.DB.QueryRowContext(ctx, query, args...)
}

// Exec runs a statement that returns no rows.
func (d *DB) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	d.echoQuery(query, args)
	return d.DB.ExecContext(ctx, query, args...)
}

package main

import (
	"database/sql"
	"fmt"

	"github.com/pressly/goose/v3"
	"github.com/rs/zerolog"

	"hepmjerenja/migrations"

	// The "sqlite" driver is registered by app/db.go.
	_ "modernc.org/sqlite"
)

// gooseLogger bridges goose's Logger interface to zerolog.
type gooseLogger struct{ logger zerolog.Logger }

func (g gooseLogger) Fatalf(format string, v ...interface{}) { g.logger.Fatal().Msgf(format, v...) }
func (g gooseLogger) Printf(format string, v ...interface{}) { g.logger.Info().Msgf(format, v...) }

// RunMigrations applies all pending Up migrations. Called on server startup.
func RunMigrations(connString string, logger zerolog.Logger) error {
	return Migrate(connString, "up", logger)
}

// Migrate opens its own short-lived *sql.DB connection (goose needs a database/sql
// handle, not the pooled wrapper), runs the requested action against the embedded
// SQL files, then closes it.
// Supported actions: "up" (apply all pending), "down" (roll back the newest
// migration), "status" (list applied/pending). Returns a non-nil error if the
// action fails or is unknown.
func Migrate(connString, action string, logger zerolog.Logger) error {
	db, err := sql.Open("sqlite", connString)
	if err != nil {
		return fmt.Errorf("open db for migrations: %w", err)
	}
	defer db.Close()

	goose.SetLogger(gooseLogger{logger: logger})
	goose.SetBaseFS(migrations.Files)

	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}

	// "." because migrations.Files is rooted at the migrations/ directory itself.
	switch action {
	case "up":
		return goose.Up(db, ".")
	case "down":
		return goose.Down(db, ".")
	case "status":
		return goose.Status(db, ".")
	default:
		return fmt.Errorf("unknown action %q — expected up, down or status", action)
	}
}

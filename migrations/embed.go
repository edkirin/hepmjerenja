package migrations

import "embed"

// Files contains all goose SQL migration files, embedded at compile time.
//
//go:embed *.sql
var Files embed.FS

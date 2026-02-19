package migrations

import "embed"

// Files contains goose migration files.
//go:embed *.sql
var Files embed.FS

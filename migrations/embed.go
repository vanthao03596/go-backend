// Package migrations embeds the SQL migration files so the binary can apply
// them at startup without the goose CLI.
package migrations

import "embed"

// FS holds every *.sql migration in this directory.
//
//go:embed *.sql
var FS embed.FS

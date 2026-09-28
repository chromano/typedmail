// Package migrations embeds the SQL schema migrations so the binary is
// self-contained and can migrate its own database on startup.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS

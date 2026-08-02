// Package migrations embeds the SQL schema migrations so binaries can
// initialise a database from zero without external tooling.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS

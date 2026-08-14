// Package migrations embeds the SQL migration files into the binary.
//
// Embedding rather than reading from disk means a deployed artifact carries
// exactly the migrations it was built and tested with. There is no way for a
// container to start against a migration directory that was updated
// separately, and no way for the migration runner to find nothing because a
// volume was not mounted.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS

// Dir is the path within FS holding the migration files.
const Dir = "."

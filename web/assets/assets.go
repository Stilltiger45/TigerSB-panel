// Package assets embeds the panel web UI into the binary.
package assets

import "embed"

// FS holds login.html, app.html and static/.
//
//go:embed login.html app.html static
var FS embed.FS

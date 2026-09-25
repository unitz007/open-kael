// Package ui embeds the compiled frontend (figma design/dist) so the API
// server can serve it without a separate static-file host.
// The dist/ directory is populated by the Dockerfile's Node build stage;
// it is gitignored so it never appears in the repo.
package ui

import (
	"embed"
	"io/fs"
)

//go:embed dist
var files embed.FS

// FS returns the dist/ subtree, rooted so callers can treat "/" as the
// web root without a "dist/" path prefix.
func FS() fs.FS {
	sub, err := fs.Sub(files, "dist")
	if err != nil {
		panic("ui: dist subtree missing from embed: " + err.Error())
	}
	return sub
}

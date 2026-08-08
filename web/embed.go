// Package web embeds the compiled admin panel into the burrowd binary.
//
// The dist directory is produced by Vite (`make web`). A placeholder
// index.html is committed so that `go build` works on a fresh checkout before
// anyone has run npm; the placeholder simply says the panel is not built yet.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist returns the compiled panel rooted at dist/.
func Dist() (fs.FS, error) {
	return fs.Sub(dist, "dist")
}

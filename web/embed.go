//go:build webbundle

package web

import (
	"embed"
	"io/fs"
)

// all: keeps files whose names begin with "_" or "." — bundlers emit shared
// chunks such as "_baseUniq-<hash>.js", and dropping them breaks diagram
// modules that dynamic-import them at runtime.
//go:embed all:dist
var assets embed.FS

// Assets returns the immutable production Web bundle rooted at dist.
func Assets() (fs.FS, error) {
	return fs.Sub(assets, "dist")
}

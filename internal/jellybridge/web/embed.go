// Package web is the bridge's pages under /sso/, written with Solid and
// built with Vite into dist, which isn't committed: build them with
// go generate before building the bridge.
package web

//go:generate npm ci
//go:generate npm run build

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS is the built pages: account.html, login.html, bridge.js, and assets/,
// whose names hold a hash of their content.
var FS = func() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}()

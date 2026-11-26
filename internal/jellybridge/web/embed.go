// Package web is the bridge's pages under /sso/, written with Solid and
// built with Vite into dist, which is committed so that building the bridge
// needs no Node: run npm ci && npm run build here after changing them.
package web

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

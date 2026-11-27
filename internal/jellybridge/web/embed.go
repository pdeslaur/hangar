// Package web is the bridge's pages under /sso/, as html/template
// templates, and the static files they use under /sso-static/, Unpoly's
// among them: nothing here is built.
package web

import (
	"embed"
	"html/template"
	"io/fs"
)

//go:embed templates/*.html
var templates embed.FS

//go:embed static
var static embed.FS

// Templates are the pages, account.html and login.html, each a whole
// document.
var Templates = template.Must(template.ParseFS(templates, "templates/*.html"))

// Static is the files under /sso-static/: bridge.js, style.css, and
// Unpoly's unpoly.min.js and unpoly.min.css.
var Static = func() fs.FS {
	sub, err := fs.Sub(static, "static")
	if err != nil {
		panic(err)
	}
	return sub
}()

package dashboard

import (
	"embed"
	"html/template"
)

// templatesFS embeds every page template. Each page is parsed together with
// base.html only (see mustParse), so the "content" template each page file
// defines never collides with another page's — every *template.Template
// built by mustParse is its own private namespace of exactly two files.
//
//go:embed templates/*.html
var templatesFS embed.FS

// staticFS embeds vendored static assets served verbatim (GET
// /static/idiomorph-<version>.min.js, server.go's idiomorphURL/handleStatic)
// — kept in a sibling directory rather than templates/ so templatesFS's
// *.html glob (and mustParse, which parses every embedded file as a Go
// template) never has to reason about a non-template file living alongside
// the pages. Currently just idiomorph (auto-refresh's DOM-morphing library,
// base.html) — see the vendored file's own header comment for
// version/license/source, and server.go's idiomorphVersion for the single
// place a re-vendor updates.
//
//go:embed static/*.js
var staticFS embed.FS

// templateFuncs are available to every page: shortSHA and compactRef let a
// template render a compact form of a SHA/ref inline while keeping the full
// value available for a title tooltip (e.g. `title="{{.SHA}}"` next to
// `{{shortSHA .SHA}}`) — see server.go's docs on both for why this exists
// (a full 40-char SHA overflows a card).
var templateFuncs = template.FuncMap{
	"shortSHA":   shortSHA,
	"compactRef": compactRef,
}

// mustParse builds a template set from base.html plus one page file and
// panics on error — template syntax errors are a programmer mistake, caught
// at package init (and by the tests that exercise every page), never a
// runtime condition.
func mustParse(page string) *template.Template {
	return template.Must(template.New(page).Funcs(templateFuncs).ParseFS(templatesFS, "templates/base.html", "templates/"+page))
}

var (
	indexTmpl  = mustParse("index.html")
	targetTmpl = mustParse("target.html")
	runTmpl    = mustParse("run.html")
	batchTmpl  = mustParse("batch.html")
	checksTmpl = mustParse("checks.html")
)

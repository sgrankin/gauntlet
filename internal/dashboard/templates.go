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

// mustParse builds a template set from base.html plus one page file and
// panics on error — template syntax errors are a programmer mistake, caught
// at package init (and by the tests that exercise every page), never a
// runtime condition.
func mustParse(page string) *template.Template {
	return template.Must(template.ParseFS(templatesFS, "templates/base.html", "templates/"+page))
}

var (
	indexTmpl  = mustParse("index.html")
	targetTmpl = mustParse("target.html")
	runTmpl    = mustParse("run.html")
	checksTmpl = mustParse("checks.html")
)

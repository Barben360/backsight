package dashboard

import (
	"bytes"
	"embed"
	"html/template"
	"io/fs"
)

// templateFS holds the dashboard's HTML templates. Each file starts with
// a views header: the views a page reads, or "none".
//
//go:embed templates/*.html
var templateFS embed.FS

// Templates is the dashboard's template files, at the root of the FS.
func Templates() fs.FS {
	sub, err := fs.Sub(templateFS, "templates")
	if err != nil {
		panic(err)
	}
	return sub
}

// unauthorizedPage is the 401 page's body, rendered once: it holds no
// data, only the asset paths.
var unauthorizedPage = renderStatic("unauthorized.html")

// renderStatic renders a template that reads only the frame's assets. A
// template that fails to parse or render is a programming error.
func renderStatic(name string) []byte {
	t := template.Must(template.New(name).Funcs(funcs).Option("missingkey=error").ParseFS(templateFS, "templates/"+name))
	var b bytes.Buffer
	if err := t.Execute(&b, PageData{Frame: Frame{Assets: frameAssets()}}); err != nil {
		panic(err)
	}
	return b.Bytes()
}

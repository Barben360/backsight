package dashboard

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// assetFS holds the dashboard's static files: the logos, copies of
// docs/assets/, and the stylesheet.
//
//go:embed assets/*
var assetFS embed.FS

// asset is one embedded file, served under its hashed name.
type asset struct {
	body  []byte
	ctype string
}

// assetByPath maps "/assets/<hashed name>" to its file; assetPaths,
// "logo-light.svg" to that path. Package variables, not init: routes
// needs them.
var assetByPath, assetPaths = loadAssets()

// assetTypes are the Content-Types of the embedded files' extensions.
var assetTypes = map[string]string{".svg": "image/svg+xml", ".css": "text/css; charset=utf-8"}

// loadAssets hashes every embedded file. A file whose extension has no
// Content-Type is a programming error.
func loadAssets() (map[string]asset, map[string]string) {
	byPath, paths := map[string]asset{}, map[string]string{}
	entries, err := fs.ReadDir(assetFS, "assets")
	if err != nil {
		panic(err)
	}
	for _, e := range entries {
		b, err := assetFS.ReadFile("assets/" + e.Name())
		if err != nil {
			panic(err)
		}
		ctype, ok := assetTypes[path.Ext(e.Name())]
		if !ok {
			panic("dashboard asset " + e.Name() + ": no Content-Type for its extension")
		}
		p := "/assets/" + hashedName(e.Name(), b)
		byPath[p] = asset{body: b, ctype: ctype}
		paths[e.Name()] = p
	}
	return byPath, paths
}

// hashedName is name with the first 12 hex characters of body's SHA-256
// before its extension: a changed file gets a new name, so a cached copy
// can live forever.
func hashedName(name string, body []byte) string {
	sum := sha256.Sum256(body)
	ext := path.Ext(name)
	return strings.TrimSuffix(name, ext) + "." + hex.EncodeToString(sum[:])[:12] + ext
}

// assetPath is the path an embedded file is served at,
// "/assets/<base>.<12 hex>.<ext>". An unknown name is a programming error.
func assetPath(name string) string {
	p, ok := assetPaths[name]
	if !ok {
		panic("dashboard: no asset " + strconv.Quote(name))
	}
	return p
}

// serveAsset serves an embedded file at its exact hashed path, as
// requested (no decoding, no cleaning); anything else is 404.
func serveAsset(w http.ResponseWriter, r *http.Request) {
	a, ok := assetByPath[r.URL.EscapedPath()]
	if !ok {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	securityHeaders(h, true)
	h.Set("Content-Type", a.ctype)
	h.Set("Content-Length", strconv.Itoa(len(a.body)))
	_, _ = w.Write(a.body)
}

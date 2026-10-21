// Package panel serves the web control panel, a single-page app built from
// web/panel and embedded in the binary (internal/panel/dist).
package panel

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var dist embed.FS

// contentSecurityPolicy keeps the panel to its own origin. hls.js runs in
// a blob: worker and plays media from blob: URLs; styles allow inline
// attributes used by the UI.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob:; media-src 'self' blob:; worker-src 'self' blob:; connect-src 'self'; " +
	"font-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// Handler serves the panel under prefix (for example "/panel/"). Unknown
// paths return index.html so the app's own routes work on reload.
func Handler(prefix string) http.Handler {
	root, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(root))
	index, _ := fs.ReadFile(root, "index.html")

	return http.StripPrefix(strings.TrimSuffix(prefix, "/"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")

		p := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if p != "" && p != "index.html" {
			if st, err := fs.Stat(root, p); err == nil && !st.IsDir() {
				if strings.HasPrefix(p, "assets/") {
					// Vite puts a content hash in asset names.
					h.Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
			if strings.HasPrefix(p, "assets/") {
				http.NotFound(w, r)
				return
			}
		}
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
		w.Write(index) //nolint:errcheck
	}))
}

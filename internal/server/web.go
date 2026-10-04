package server

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed web
var webFS embed.FS

// webHandler serves the embedded single-page UI. Unknown non-asset paths
// get index.html so client-side routes (/setup, /pair, /w/<id>) work.
func webHandler() http.Handler {
	sub, _ := fs.Sub(webFS, "web")
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(rw, r)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" || !strings.Contains(p, ".") {
			rw.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self' ws: wss:; img-src 'self' data:")
			rw.Header().Set("Cache-Control", "no-cache")
			b, _ := webFS.ReadFile("web/index.html")
			rw.Header().Set("Content-Type", "text/html; charset=utf-8")
			rw.Write(b)
			return
		}
		files.ServeHTTP(rw, r)
	})
}

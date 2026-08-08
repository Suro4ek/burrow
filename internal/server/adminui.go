package server

import (
	"io/fs"
	"net/http"
	"strings"

	"github.com/suro4ek/burrow/web"
)

// adminUI serves the compiled panel from the binary.
//
// Hashed asset filenames are safe to cache forever; index.html must not be,
// or a redeploy would leave browsers loading an old bundle that references
// assets which no longer exist.
func (srv *Server) adminUI() http.Handler {
	dist, err := web.Dist()
	if err != nil {
		srv.log.Error("admin UI assets unavailable", "err", err)
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeErrorPage(w, http.StatusInternalServerError, "Admin UI is not built",
				"Run `make web` (or `npm --prefix web ci && npm --prefix web run build`) and rebuild burrowd.")
		})
	}

	files := http.FileServerFS(dist)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upath := strings.TrimPrefix(r.URL.Path, "/")
		if upath == "" {
			upath = "index.html"
		}

		if _, err := fs.Stat(dist, upath); err != nil {
			// Unknown path: hand it to the SPA rather than 404, so a deep
			// link or a refresh on /_admin/tokens still loads the app.
			serveIndex(w, r, dist)
			return
		}
		if strings.HasPrefix(upath, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

// serveIndex writes the SPA shell.
func serveIndex(w http.ResponseWriter, r *http.Request, dist fs.FS) {
	b, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		writeErrorPage(w, http.StatusInternalServerError, "Admin UI is not built",
			"index.html is missing from the embedded assets.")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	// A 200 is correct here: the SPA owns client-side routing, and the shell
	// itself was found.
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(b)
}

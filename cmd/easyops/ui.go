package main

import (
	"embed"
	"io/fs"
	"net/http"
)

// The web UI (Svelte 5 + Vite + Tailwind, from the easyops-webui repo,
// github.com/easydev-org/easyops-webui) is embedded so the binary serves it with
// no external files. `all:` includes the Vite hashed assets. ui/dist is built by
// the Containerfile's node stage (it is NOT committed).
//
//go:embed all:ui/dist
var uiDist embed.FS

// mountUI serves the embedded SPA at /ui/. It is unauthenticated: the SPA calls
// the ops.v1 Connect API same-origin, and the operator pastes the service token
// (kept in localStorage). Unknown sub-paths fall back to index.html.
func mountUI(mux *http.ServeMux) {
	sub, err := fs.Sub(uiDist, "ui/dist")
	if err != nil {
		return
	}
	fileServer := http.FileServerFS(sub)
	mux.HandleFunc("GET /ui", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /ui/", func(w http.ResponseWriter, r *http.Request) {
		if p := r.URL.Path[len("/ui/"):]; p != "" {
			if _, err := fs.Stat(sub, p); err != nil {
				r = r.Clone(r.Context())
				r.URL.Path = "/ui/"
			}
		}
		http.StripPrefix("/ui/", fileServer).ServeHTTP(w, r)
	})
}

// Package webassets embeds the built SvelteKit SPA so the control plane serves
// its own UI out of a single static binary (CICD.md §1). The build order
// matters: `pnpm build` must populate dist/ before `go build`, or an empty tree
// gets embedded — see the web-build target in the Makefile and stage 1 of the
// Dockerfile.
package webassets

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// all: so hashed asset directories starting with _ (SvelteKit's _app) are
// included; a plain embed would silently skip them and serve a blank page.
//
//go:embed all:dist
var embedded embed.FS

// Available reports whether a real UI was embedded. A checkout that never ran
// the frontend build still compiles (dist/ holds a .gitkeep placeholder), so
// callers must be able to tell the difference rather than serving 404s that
// look like a routing bug.
func Available() bool {
	_, err := fs.Stat(dist(), "index.html")
	return err == nil
}

func dist() fs.FS {
	sub, err := fs.Sub(embedded, "dist")
	if err != nil {
		// Only possible if the embed directive above is malformed.
		panic("webassets: " + err.Error())
	}
	return sub
}

// Handler serves the embedded SPA. Unknown paths fall back to index.html so
// client-side routes (/machines/:id, …) survive a hard refresh; missing files
// under the hashed asset prefix 404 normally instead, since serving HTML in
// place of a stale .js chunk produces a confusing MIME error in the browser.
func Handler() http.Handler {
	root := dist()
	files := http.FileServer(http.FS(root))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !Available() {
			http.Error(w, "UI not embedded in this build", http.StatusNotFound)
			return
		}

		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			serveIndex(w, root)
			return
		}

		if _, err := fs.Stat(root, name); err == nil {
			// Fingerprinted bundles are immutable; everything else stays
			// revalidated so a redeploy is picked up without a hard refresh.
			if strings.HasPrefix(name, "_app/immutable/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			files.ServeHTTP(w, r)
			return
		}

		if strings.HasPrefix(name, "_app/") {
			http.NotFound(w, r)
			return
		}

		serveIndex(w, root)
	})
}

func serveIndex(w http.ResponseWriter, root fs.FS) {
	index, err := fs.ReadFile(root, "index.html")
	if err != nil {
		http.Error(w, "UI not embedded in this build", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The shell references hashed bundles, so it must never be cached stale.
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(index)
}

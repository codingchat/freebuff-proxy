//go:build !dashboard

package dashboard

import (
	"fmt"
	"io/fs"
	"net/http"
	"testing/fstest"
)

// HasEmbeddedSPA reports whether the binary was compiled with the embedded web dashboard.
const HasEmbeddedSPA = false

// DistFS returns a stub filesystem for CLI-only builds.
func DistFS() fs.FS {
	return fstest.MapFS{
		"assets": &fstest.MapFile{Mode: fs.ModeDir},
	}
}

// ServeSPA serves a lightweight plain-text notice when accessed without embedded web dashboard.
func (d *Dashboard) ServeSPA(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "freebuff-proxy is running in CLI mode (compiled without -tags dashboard).\n"+
		"All proxy endpoints (/v1/chat/completions, /v1/models, /healthz, /metrics, /admin/api/*) are active.\n")
}

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMountUI: the embedded SPA is served at /ui/ (index.html at the root, a
// hashed asset by path, and index.html for unknown sub-paths — the SPA
// fallback), and /ui redirects to /ui/.
func TestMountUI(t *testing.T) {
	mux := http.NewServeMux()
	mountUI(mux)

	// /ui -> /ui/ (permanent redirect).
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ui", nil))
	if rr.Code != http.StatusMovedPermanently || rr.Header().Get("Location") != "/ui/" {
		t.Fatalf("/ui: code=%d location=%q", rr.Code, rr.Header().Get("Location"))
	}

	// /ui/ -> index.html (contains the app mount point).
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ui/", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `id="app"`) {
		t.Fatalf("/ui/: code=%d body=%q", rr.Code, rr.Body.String())
	}

	// An unknown SPA route falls back to index.html (not 404).
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ui/some/deep/route", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `id="app"`) {
		t.Fatalf("SPA fallback: code=%d body=%q", rr.Code, rr.Body.String())
	}
}

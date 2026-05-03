package dashboard_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/schjan/picolet/pkg/dashboard"
)

func TestHandler_ServesPlaceholder(t *testing.T) {
	t.Parallel()

	h, err := dashboard.NewHandler(nil, nil, nil, "0.0.0-test", nil)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	mux := http.NewServeMux()
	h.Register(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("content-type = %q, want text/html...", got)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "picolet") {
		t.Fatalf("body missing 'picolet'; got %q", body)
	}
}

func TestHandler_404OnNonRoot(t *testing.T) {
	t.Parallel()
	h, _ := dashboard.NewHandler(nil, nil, nil, "0.0.0-test", nil)
	mux := http.NewServeMux()
	h.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/totally-bogus", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHandler_HTMLCacheControl(t *testing.T) {
	t.Parallel()
	h, _ := dashboard.NewHandler(nil, nil, nil, "0.0.0", nil)
	mux := http.NewServeMux()
	h.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("HTML Cache-Control = %q, want no-store", got)
	}
}

func TestHandler_StaticAssetServed(t *testing.T) {
	t.Parallel()
	h, _ := dashboard.NewHandler(nil, nil, nil, "0.0.0", nil)
	mux := http.NewServeMux()
	h.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/picolet.css", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/css") {
		t.Errorf("content-type = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=86400" {
		t.Errorf("static Cache-Control = %q", got)
	}
}

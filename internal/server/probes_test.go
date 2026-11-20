package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

func TestNewProbeHandlerSetsContentTypeOptions(t *testing.T) {
	handler := NewProbeHandler(
		map[string]healthz.Checker{"healthz": healthz.Ping},
		map[string]healthz.Checker{"readyz": healthz.Ping},
	)

	tests := []struct {
		name string
		path string
	}{
		{name: "aggregated liveness endpoint", path: "/healthz"},
		{name: "named liveness sub-check", path: "/healthz/healthz"},
		{name: "aggregated readiness endpoint", path: "/readyz"},
		{name: "named readiness sub-check", path: "/readyz/readyz"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)

			handler.ServeHTTP(rec, req)

			res := rec.Result()
			defer func() { _ = res.Body.Close() }()

			if res.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusOK)
			}
			if got := res.Header.Get(contentTypeOptionsHeader); got != contentTypeOptionsValue {
				t.Fatalf("%s = %q, want %q", contentTypeOptionsHeader, got, contentTypeOptionsValue)
			}
			if got := rec.Body.String(); got != "ok" {
				t.Fatalf("body = %q, want %q", got, "ok")
			}
		})
	}
}

func TestNewProbeHandlerSetsHeaderOnFailure(t *testing.T) {
	failing := func(_ *http.Request) error { return errors.New("not ready") }
	handler := NewProbeHandler(
		map[string]healthz.Checker{"healthz": healthz.Ping},
		map[string]healthz.Checker{"readyz": failing},
	)

	tests := []struct {
		name string
		path string
	}{
		{name: "aggregated readiness endpoint", path: "/readyz"},
		{name: "named readiness sub-check", path: "/readyz/readyz"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)

			handler.ServeHTTP(rec, req)

			res := rec.Result()
			defer func() { _ = res.Body.Close() }()

			if res.StatusCode != http.StatusInternalServerError {
				t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusInternalServerError)
			}
			if got := res.Header.Get(contentTypeOptionsHeader); got != contentTypeOptionsValue {
				t.Fatalf("%s = %q, want %q", contentTypeOptionsHeader, got, contentTypeOptionsValue)
			}
		})
	}
}

func TestNewProbeHandlerUnknownPath(t *testing.T) {
	handler := NewProbeHandler(
		map[string]healthz.Checker{"healthz": healthz.Ping},
		map[string]healthz.Checker{"readyz": healthz.Ping},
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/does-not-exist", nil)

	handler.ServeHTTP(rec, req)

	res := rec.Result()
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusNotFound)
	}
}

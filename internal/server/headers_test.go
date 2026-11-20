package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWithContentTypeOptions(t *testing.T) {
	tests := []struct {
		name    string
		handler http.Handler
		want    int
	}{
		{
			name: "sets header on success response",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
			want: http.StatusOK,
		},
		{
			name: "sets header on error response",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "boom", http.StatusInternalServerError)
			}),
			want: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", nil)

			WithContentTypeOptions(tt.handler).ServeHTTP(rec, req)

			res := rec.Result()
			defer func() { _ = res.Body.Close() }()

			if res.StatusCode != tt.want {
				t.Fatalf("status = %d, want %d", res.StatusCode, tt.want)
			}
			if got := res.Header.Get(contentTypeOptionsHeader); got != contentTypeOptionsValue {
				t.Fatalf("%s = %q, want %q", contentTypeOptionsHeader, got, contentTypeOptionsValue)
			}
		})
	}
}

func TestWithStrictTransportSecurity(t *testing.T) {
	tests := []struct {
		name    string
		handler http.Handler
		want    int
	}{
		{
			name: "sets header on success response",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
			want: http.StatusOK,
		},
		{
			// Mirrors the production ordering in cmd's metricsAuthWithHSTS, where the
			// HSTS middleware wraps the authentication filter: the header must be
			// flushed to the client even when the inner handler short-circuits with
			// a 401 before any body is written.
			name: "sets header on unauthorized response",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
			}),
			want: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/metrics", nil)

			WithStrictTransportSecurity(tt.handler).ServeHTTP(rec, req)

			res := rec.Result()
			defer func() { _ = res.Body.Close() }()

			if res.StatusCode != tt.want {
				t.Fatalf("status = %d, want %d", res.StatusCode, tt.want)
			}
			if got := res.Header.Get(strictTransportSecurityHeader); got != strictTransportSecurityValue {
				t.Fatalf("%s = %q, want %q", strictTransportSecurityHeader, got, strictTransportSecurityValue)
			}
		})
	}
}

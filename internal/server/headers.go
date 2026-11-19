package server

import "net/http"

const (
	// contentTypeOptionsHeader is the response header that disables browser
	// MIME-type sniffing.
	contentTypeOptionsHeader = "X-Content-Type-Options"
	// contentTypeOptionsValue is the only valid value for the
	// X-Content-Type-Options header.
	contentTypeOptionsValue = "nosniff"

	// strictTransportSecurityHeader is the HSTS response header that instructs
	// compliant clients to only communicate over HTTPS.
	strictTransportSecurityHeader = "Strict-Transport-Security"
	// strictTransportSecurityValue enables HSTS for one year, including
	// subdomains. The endpoints this is applied to are machine-to-machine and
	// dedicated to the operator, so includeSubDomains has no sibling impact.
	strictTransportSecurityValue = "max-age=31536000; includeSubDomains"
)

// WithContentTypeOptions wraps next so every response carries
// X-Content-Type-Options: nosniff. The header is set before next is invoked so
// it is present on success, error, and short-circuited responses alike.
func WithContentTypeOptions(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(contentTypeOptionsHeader, contentTypeOptionsValue)
		next.ServeHTTP(w, r)
	})
}

// WithStrictTransportSecurity wraps next so every response carries the HSTS
// header. It is intended for HTTPS surfaces only; the header is set before next
// is invoked so it is present even on responses an inner authentication filter
// short-circuits (e.g. 401/403).
func WithStrictTransportSecurity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(strictTransportSecurityHeader, strictTransportSecurityValue)
		next.ServeHTTP(w, r)
	})
}

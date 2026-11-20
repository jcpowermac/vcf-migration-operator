package server

import (
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

const (
	// livenessEndpoint is the path the liveness probe handler is mounted at. It
	// matches controller-runtime's default liveness endpoint name.
	livenessEndpoint = "/healthz"
	// readinessEndpoint is the path the readiness probe handler is mounted at. It
	// matches controller-runtime's default readiness endpoint name.
	readinessEndpoint = "/readyz"
)

// NewProbeHandler builds the HTTP handler for the liveness and readiness probe
// endpoints. It mirrors controller-runtime's manager routing, serving both the
// aggregated endpoint (e.g. /healthz) and the named sub-check endpoints (e.g.
// /healthz/<name>), and wraps every response with the X-Content-Type-Options:
// nosniff header.
//
// controller-runtime sets that header on the aggregated responses but not on the
// named sub-check responses, which this handler closes by applying the header
// across all probe paths. A nil or empty check map leaves the corresponding
// endpoint unregistered.
func NewProbeHandler(healthzChecks, readyzChecks map[string]healthz.Checker) http.Handler {
	mux := http.NewServeMux()

	if len(healthzChecks) > 0 {
		mountProbe(mux, livenessEndpoint, &healthz.Handler{Checks: healthzChecks})
	}
	if len(readyzChecks) > 0 {
		mountProbe(mux, readinessEndpoint, &healthz.Handler{Checks: readyzChecks})
	}

	return WithContentTypeOptions(mux)
}

// mountProbe registers handler at endpoint and its sub-paths, stripping the
// endpoint prefix so the healthz.Handler sees the aggregated root and named
// sub-checks, matching controller-runtime's manager behavior.
func mountProbe(mux *http.ServeMux, endpoint string, handler http.Handler) {
	mux.Handle(endpoint, http.StripPrefix(endpoint, handler))
	// Append '/' suffix to handle sub-paths (named checks).
	mux.Handle(endpoint+"/", http.StripPrefix(endpoint, handler))
}

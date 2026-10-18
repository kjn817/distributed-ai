// Package dashboard serves the public network dashboard: a single static
// page reading the service's live JSON APIs. No mocks — every number comes
// from real heartbeats and request logs.
package dashboard

import (
	_ "embed"
	"net/http"
)

//go:embed dashboard.html
var page []byte

// Version is the service version, overridden at release builds via
// -ldflags "-X distributed-ai/service/internal/dashboard.Version=x.y.z".
var Version = "0.1.0-alpha"

// Handler serves the dashboard page.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(page)
	})
}

// VersionInfo is the /v1/version payload: the anchor for future auto-update.
func VersionInfo() map[string]any {
	return map[string]any{"version": Version, "service": "distributed-ai"}
}

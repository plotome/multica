package main

import (
	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/realtime"
	"net/http"
	"testing"
)

func TestDeviceMaintenanceRegisteredInRealRouter(t *testing.T) {
	r := NewRouter(nil, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil)
	found := map[string]bool{}
	err := chi.Walk(r, func(method, path string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if path == "/api/device-maintenance/{daemonId}/{action}" {
			found[method] = true
		}
		return nil
	})
	if err != nil || !found["GET"] || !found["POST"] {
		t.Fatal("device maintenance missing from authenticated production router", err, found)
	}
}

package main

import (
	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/realtime"
	"net/http"
	"testing"
)

// Walk the real application router: a fixture mounting a handler directly
// cannot catch a private artifact handler missing from production routing.
func TestForkUpdatePrivateArtifactIsRegisteredInRealRouter(t *testing.T) {
	router := NewRouter(nil, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil)
	found := false
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodGet && route == "/api/daemon/runtimes/{runtimeId}/fork-update/assets/{digest}" {
			found = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("private artifact handler is missing from the real daemon router")
	}
}

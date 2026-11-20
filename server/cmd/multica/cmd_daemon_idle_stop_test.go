package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIdleShutdownRequestPinsProcessAndPropagatesBusy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/shutdown" || r.URL.Query().Get("when_idle") != "1" || r.URL.Query().Get("expected_pid") != "42" {
			t.Error("wrong idle shutdown request")
		}
		w.WriteHeader(http.StatusConflict)
	}))
	defer server.Close()
	if err := requestDaemonIdleShutdown(server.Listener.Addr().(*net.TCPAddr).Port, 42); err == nil {
		t.Fatal("busy daemon was treated as stopped")
	}
}

func TestRunningDaemonCommitRequiresFullEmbeddedRevision(t *testing.T) {
	full := strings.Repeat("b", 40)
	for _, tc := range []struct{ local, release, want string }{{"unknown", full, full}, {"short", full, full}, {full, "unknown", full}, {"v1.2.3", "1234567", "unknown"}, {strings.Repeat("x", 40), "unknown", "unknown"}} {
		if got := runningDaemonCommit(tc.local, tc.release); got != tc.want {
			t.Fatalf("source identity %q, %q: got %q want %q", tc.local, tc.release, got, tc.want)
		}
	}
}

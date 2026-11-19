//go:build device_consumer

package devicemaintenance

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// This explicit integration target executes the real Python consumer against
// the same Store/Serve implementation and existing isolated PostgreSQL schema.
// Native App/launchd operations are replaced only in that consumer fixture.
func TestDeviceMaintenanceConsumerClosedLoop(t *testing.T) {
	pool, fx, device, _ := fixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 4 || parts[0] != "api" || parts[1] != "device-maintenance" || parts[2] != device {
			http.NotFound(w, r)
			return
		}
		(Store{pool}).Serve(w, r, device, fx.UserID, parts[3])
	}))
	defer server.Close()
	python, consumer := os.Getenv("DEVICE_CONSUMER_PYTHON"), os.Getenv("DEVICE_CONSUMER_SCRIPT")
	if python == "" || consumer == "" {
		t.Fatal("explicit DEVICE_CONSUMER_PYTHON and DEVICE_CONSUMER_SCRIPT required")
	}
	cmd := exec.Command(python, consumer, server.URL, device)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated consumer: %v\n%s", err, output)
	}
	t.Log(string(output))
}

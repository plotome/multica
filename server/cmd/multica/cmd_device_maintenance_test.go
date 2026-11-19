package main

import (
	"github.com/google/uuid"
	"strings"
	"testing"
)

func TestDeviceMaintenanceRefusesDaemonTaskMutations(t *testing.T) {
	t.Setenv("MULTICA_TASK_ID", "isolated-fixture")
	for _, action := range []string{"acquire", "drain", "release"} {
		cmd, args, err := rootCmd.Find([]string{"device", "maintenance", action, uuid.NewString()})
		if err != nil {
			t.Fatal(err)
		}
		if err = cmd.RunE(cmd, args); err == nil || !strings.Contains(err.Error(), "not available inside a daemon-managed task") {
			t.Fatal("task mutation was not rejected before any API request", action, err)
		}
	}
}

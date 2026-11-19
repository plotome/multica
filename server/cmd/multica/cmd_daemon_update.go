package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

func init() {
	for _, resume := range []bool{false, true} {
		name := "prepare-update"
		if resume {
			name = "resume-update"
		}
		cmd := &cobra.Command{Use: name, Short: "Hold or release the durable idle-only update maintenance gate", Args: cobra.NoArgs}
		cmd.Flags().String("lease", "", "Updater-owned UUID, persisted with the transaction")
		cmd.Flags().String("digest", "", "Approved candidate SHA-256 digest")
		cmd.Flags().String("expected-commit", "", "Expected full revision of the running daemon")
		for _, flag := range []string{"lease", "digest", "expected-commit"} {
			_ = cmd.MarkFlagRequired(flag)
		}
		cmd.RunE = func(cmd *cobra.Command, _ []string) error { return runDaemonUpdateMaintenance(cmd, resume) }
		daemonCmd.AddCommand(cmd)
	}
}
func runDaemonUpdateMaintenance(cmd *cobra.Command, resume bool) error {
	if err := requireHumanLocalCommand("daemon " + cmd.Name()); err != nil {
		return err
	}
	profile := resolveProfile(cmd)
	if err := requireKnownProfile(profile); err != nil {
		return err
	}
	lease, _ := cmd.Flags().GetString("lease")
	digest, _ := cmd.Flags().GetString("digest")
	revision, _ := cmd.Flags().GetString("expected-commit")
	if _, err := uuid.Parse(lease); err != nil || !isLowerHex(digest, 64) || !isLowerHex(revision, 40) {
		return fmt.Errorf("lease UUID, candidate digest and full revision are required")
	}
	port := resolveHealthPort(profile)
	ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
	defer cancel()
	health := checkDaemonHealthOnPort(ctx, port)
	if !daemonAlive(health) {
		return fmt.Errorf("daemon not reachable; no maintenance state changed")
	}
	if err := daemonIdentityMismatch(health, profile, port); err != nil {
		return err
	}
	// Unlike legacy lifecycle commands, missing profile/capability is not proof.
	if health["profile"] != profile || health["update_maintenance_supported"] != true || health["cli_commit"] != revision {
		return fmt.Errorf("daemon does not attest the expected profile, revision and durable update gate")
	}
	pid, ok := health["pid"].(float64)
	if !ok || pid <= 0 {
		return fmt.Errorf("daemon PID is not attested")
	}
	path := "prepare"
	if resume {
		path = "resume"
	}
	payload, _ := json.Marshal(map[string]any{"id": lease, "digest": digest, "pid": int(pid), "commit": revision})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/updates/%s", port, path), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("local maintenance redirect refused") }}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil {
		return err
	}
	if response.StatusCode == http.StatusConflict && strings.TrimSpace(string(data)) == "daemon is busy" {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{"state": "waiting", "reason": "busy"})
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon update gate refused (HTTP %d): %s", response.StatusCode, strings.TrimSpace(string(data)))
	}
	var result struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(data, &result) != nil || result.Status != "ok" {
		return fmt.Errorf("unrecognized maintenance acknowledgement")
	}
	state := "paused"
	if resume {
		state = "resumed"
	}
	return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{"state": state, "lease": lease, "digest": digest})
}
func isLowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

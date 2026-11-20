package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/forkupdate"
)

func TestForkUpdateCLIPlansOnlyVerifiedCandidate(t *testing.T) {
	dir := t.TempDir()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	artifact := forkupdate.Artifact{ID: "desktop", Host: "client", Kind: "desktop", Platform: "darwin", Arch: "arm64", Version: "0.8.1-fork.2", Commit: strings.Repeat("b", 40), URL: "https://updates.example/package", SHA256: strings.Repeat("a", 64), Bytes: 1}
	artifact.ComponentVersions = map[string]string{"desktop": artifact.Version, "cli": artifact.Version, "daemon": artifact.Version}
	payload, _ := json.Marshal(forkupdate.Catalog{Schema: 1, Channel: "selfhost", Sequence: 2, Artifacts: []forkupdate.Artifact{artifact}})
	envelope, _ := json.Marshal(forkupdate.Envelope{Payload: base64.StdEncoding.EncodeToString(payload), Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(private, payload))})
	keyPath, catalogPath := filepath.Join(dir, "public-key"), filepath.Join(dir, "catalog.json")
	inventoryPath, receipt := filepath.Join(dir, "inventory.json"), filepath.Join(dir, "receipt.json")
	inventory, _ := json.Marshal(map[string]forkupdate.Baseline{"desktop": {Host: "client", Commit: strings.Repeat("a", 40), Sequence: 1, Platform: "darwin", Arch: "arm64"}})
	for path, data := range map[string][]byte{keyPath: []byte(base64.StdEncoding.EncodeToString(public)), catalogPath: envelope, inventoryPath: inventory} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	group, _, err := rootCmd.Find([]string{"fork-update"})
	if err != nil {
		t.Fatal(err)
	}
	plan, _, err := group.Find([]string{"plan"})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	plan.SetOut(&out)
	t.Cleanup(func() { plan.SetOut(nil) })
	args := []string{"--catalog", catalogPath, "--public-key", keyPath, "--channel", "selfhost", "--origin", "https://updates.example", "--inventory", inventoryPath, "--receipt", receipt}
	if err := plan.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	if err := plan.RunE(plan, nil); err != nil {
		t.Fatal(err)
	}
	r, err := (forkupdate.Journal{Path: receipt}).Read()
	if err != nil || r.Approved || r.Complete() || r.Targets["desktop"].Artifact.Commit != artifact.Commit {
		t.Fatalf("unsafe prepared plan: %#v, %v", r, err)
	}
	if err := plan.RunE(plan, nil); err == nil {
		t.Fatal("overwrote existing receipt")
	}
	if err := plan.Flags().Set("channel", "official"); err != nil {
		t.Fatal(err)
	}
	if err := plan.RunE(plan, nil); err == nil {
		t.Fatal("accepted wrong channel")
	}
}

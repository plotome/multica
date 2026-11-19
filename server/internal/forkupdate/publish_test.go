package forkupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublisherBindsFullGatePackagesAndIsolatedSource(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", source}, args...)...)
		data, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("fixture git: %v %s", err, data)
		}
		return strings.TrimSpace(string(data))
	}
	git("init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(source, "tracked"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "--", "tracked")
	git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "fixture source", "--", "tracked")
	git("remote", "add", "origin", ForkSourceURL)
	head := git("rev-parse", "HEAD")
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(root, "private-key")
	if err := os.WriteFile(key, []byte(base64.StdEncoding.EncodeToString(private)), 0600); err != nil {
		t.Fatal(err)
	}
	packages := filepath.Join(root, "packages")
	os.Mkdir(packages, 0700)
	catalog := catalogFixture()
	catalog.Channel = "selfhost-test"
	for i := range catalog.Artifacts {
		catalog.Artifacts[i].Commit = head
		if _, err := os.Stat(filepath.Join(packages, catalog.Artifacts[i].SHA256)); err == nil {
			continue
		}
		if err := os.WriteFile(filepath.Join(packages, catalog.Artifacts[i].SHA256), []byte("package"), 0400); err != nil {
			t.Fatal(err)
		}
	}
	gate := filepath.Join(root, "gate.log")
	text := "selfhost integrity PASS: " + head + "; 11 accepted patches\n"
	for _, pkg := range []string{"internal/daemon", "internal/daemon/execenv", "internal/daemon/repocache", "pkg/agent"} {
		text += "ok  github.com/multica-ai/multica/server/" + pkg + "\t1s\n"
	}
	os.WriteFile(gate, []byte(text), 0600)
	output := filepath.Join(root, "catalog.json")
	publish := func(c Catalog, isolation bool) error {
		return Publish(context.Background(), source, gate, packages, key, output, "https://updates.example", c, isolation)
	}
	if err := publish(catalog, true); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(output)
	signed, err := VerifyCatalog(data, Policy{PublicKey: public, Origin: "https://updates.example", Channel: "selfhost-test", RequireSource: true})
	if err != nil {
		t.Fatal(err)
	}
	if signed.Source.Commit != head || !signed.Source.Isolation || signed.Source.Merged {
		t.Fatal("unmerged test package became a formal candidate")
	}
	if err := publish(catalog, true); err == nil {
		t.Fatal("publisher replay accepted")
	}
	catalog.Sequence++
	os.WriteFile(gate, []byte("static gate only"), 0600)
	if err := publish(catalog, true); err == nil {
		t.Fatal("partial gate evidence accepted")
	}
	os.WriteFile(gate, []byte(text), 0600)
	catalog.Channel = "selfhost"
	if err := publish(catalog, false); err == nil {
		t.Fatal("unmerged source entered formal channel")
	}
	after, _ := os.ReadFile(output)
	if string(after) != string(data) {
		t.Fatal("failed publication replaced good catalog")
	}
}
func TestLoopbackPrivateOriginAndRemotePlaintextRejection(t *testing.T) {
	catalog := catalogFixture()
	for i := range catalog.Artifacts {
		catalog.Artifacts[i].URL = "http://127.0.0.1:18157/package"
	}
	data, policy := signFixture(t, catalog)
	policy.Origin = "http://127.0.0.1:18157"
	if _, err := VerifyCatalog(data, policy); err != nil {
		t.Fatal(err)
	}
	policy.Origin = "http://example.com"
	if _, err := VerifyCatalog(data, policy); err == nil {
		t.Fatal("remote plaintext artifact channel accepted")
	}
}

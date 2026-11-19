package forkupdate

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func catalogFixture() Catalog {
	sum := sha256.Sum256([]byte("package"))
	base := Artifact{ID: "mini-server", Host: "mini", Kind: "server-web", Platform: "linux", Arch: "arm64", Version: "0.8.1-fork.2", Commit: strings.Repeat("b", 40), URL: "https://updates.example/sha/package", SHA256: hex.EncodeToString(sum[:]), Bytes: 7}
	cli := base
	cli.ID, cli.Kind, cli.Platform = "mini-cli", "cli-daemon", "darwin"
	desktop := base
	desktop.ID, desktop.Host, desktop.Kind, desktop.Platform = "air-desktop", "air", "desktop", "darwin"
	return Catalog{Schema: 1, Channel: "selfhost", Sequence: 2, Artifacts: []Artifact{base, cli, desktop}}
}

func signFixture(t *testing.T, catalog Catalog) ([]byte, Policy) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(Envelope{Payload: base64.StdEncoding.EncodeToString(payload), Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(private, payload))})
	if err != nil {
		t.Fatal(err)
	}
	return envelope, Policy{PublicKey: public, Channel: "selfhost", Origin: "https://updates.example"}
}

func TestSignedForkCatalog(t *testing.T) {
	envelope, policy := signFixture(t, catalogFixture())
	got, err := VerifyCatalog(envelope, policy)
	if err != nil || len(got.Artifacts) != 3 || len(got.Digest) != 64 {
		t.Fatalf("verify = %#v, %v", got, err)
	}
	if err := VerifyArtifact(strings.NewReader("package"), got.Artifacts[0]); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{"packagE", "packag", "package-more"} {
		if err := VerifyArtifact(strings.NewReader(data), got.Artifacts[0]); err == nil {
			t.Fatalf("accepted wrong package %q", data)
		}
	}
}

func TestRejectUntrustedForkCandidates(t *testing.T) {
	for name, change := range map[string]func(*Catalog){
		"official-source": func(c *Catalog) { c.Artifacts[0].URL = "https://github.com/multica-ai/multica/package" },
		"wrong-channel":   func(c *Catalog) { c.Channel = "official" },
		"duplicate":       func(c *Catalog) { c.Artifacts[1].ID = c.Artifacts[0].ID },
		"short-commit":    func(c *Catalog) { c.Artifacts[0].Commit = "abc123" },
		"wrong-kind":      func(c *Catalog) { c.Artifacts[0].Kind = "shell" },
		"credentials":     func(c *Catalog) { c.Artifacts[0].URL = "https://user:pass@updates.example/a" },
		"http":            func(c *Catalog) { c.Artifacts[0].URL = "http://updates.example/a" },
		"zero-sequence":   func(c *Catalog) { c.Sequence = 0 },
		"empty":           func(c *Catalog) { c.Artifacts = nil },
		"zero-bytes":      func(c *Catalog) { c.Artifacts[0].Bytes = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			c := catalogFixture()
			change(&c)
			data, policy := signFixture(t, c)
			if _, err := VerifyCatalog(data, policy); err == nil {
				t.Fatal("accepted invalid catalog")
			}
		})
	}
	data, policy := signFixture(t, catalogFixture())
	var e Envelope
	_ = json.Unmarshal(data, &e)
	payload, _ := base64.StdEncoding.DecodeString(e.Payload)
	e.Payload = base64.StdEncoding.EncodeToString(bytes.Replace(payload, []byte("selfhost"), []byte("official"), 1))
	tampered, _ := json.Marshal(e)
	if _, err := VerifyCatalog(tampered, policy); err == nil {
		t.Fatal("accepted tampered payload")
	}
	_, otherPolicy := signFixture(t, catalogFixture())
	if _, err := VerifyCatalog(data, otherPolicy); err == nil {
		t.Fatal("accepted foreign signature")
	}
}

func TestPlanRejectsDowngradeWrongHostAndMissingComponents(t *testing.T) {
	c := verifiedFixture(t)
	for name, change := range map[string]func(map[string]Baseline){
		"missing":        func(i map[string]Baseline) { delete(i, "air-desktop") },
		"replay":         func(i map[string]Baseline) { b := i["air-desktop"]; b.Sequence = 2; i["air-desktop"] = b },
		"downgrade":      func(i map[string]Baseline) { b := i["air-desktop"]; b.Sequence = 3; i["air-desktop"] = b },
		"wrong-platform": func(i map[string]Baseline) { b := i["air-desktop"]; b.Platform = "linux"; i["air-desktop"] = b },
		"wrong-arch":     func(i map[string]Baseline) { b := i["air-desktop"]; b.Arch = "amd64"; i["air-desktop"] = b },
		"wrong-host":     func(i map[string]Baseline) { b := i["air-desktop"]; b.Host = "mini"; i["air-desktop"] = b },
	} {
		t.Run(name, func(t *testing.T) {
			i := inventoryFixture(c)
			change(i)
			if _, err := Plan(c, i); err == nil {
				t.Fatal("accepted unsafe plan")
			}
		})
	}
}

func verifiedFixture(t *testing.T) VerifiedCatalog {
	t.Helper()
	data, policy := signFixture(t, catalogFixture())
	c, err := VerifyCatalog(data, policy)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func inventoryFixture(c VerifiedCatalog) map[string]Baseline {
	i := map[string]Baseline{}
	for _, a := range c.Artifacts {
		i[a.ID] = Baseline{Host: a.Host, Commit: strings.Repeat("a", 40), Sequence: 1, Platform: a.Platform, Arch: a.Arch}
	}
	return i
}

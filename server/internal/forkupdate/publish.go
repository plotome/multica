package forkupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const ForkSourceURL = "https://github.com/plotome/multica"

type SourceProof struct {
	Commit     string `json:"commit"`
	Repository string `json:"repository"`
	GateSHA256 string `json:"gate_sha256"`
	Merged     bool   `json:"merged"`
	Isolation  bool   `json:"isolation"`
}

func gitValue(ctx context.Context, source string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", source}, args...)...)
	data, err := command.Output()
	return strings.TrimSpace(string(data)), err
}

// Publish consumes complete built packages and a full gate log for the exact
// clean source. Unmerged source can only be signed for the isolated test channel.
// No fetch, GitHub write, tag/release, running service or database is touched.
func Publish(ctx context.Context, source, gateLog, packageDir, privateKey, output, origin string, catalog Catalog, isolation bool) error {
	head, err := gitValue(ctx, source, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if !commitPattern.MatchString(head) {
		return errors.New("publisher requires a full source revision")
	}
	status, err := gitValue(ctx, source, "status", "--porcelain")
	if err != nil || status != "" {
		return errors.New("publisher source must be clean")
	}
	remote, err := gitValue(ctx, source, "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	if strings.TrimSuffix(remote, ".git") != ForkSourceURL && remote != "git@github.com:plotome/multica.git" {
		return errors.New("publisher source is not the selfhost fork")
	}
	if isolation != (catalog.Channel == "selfhost-test") {
		return errors.New("unmerged packages are restricted to selfhost-test")
	}
	if !isolation {
		if _, err := gitValue(ctx, source, "merge-base", "--is-ancestor", head, "origin/main"); err != nil {
			return errors.New("formal channel requires source merged into origin/main")
		}
	}
	gate, err := os.ReadFile(gateLog)
	if err != nil {
		return err
	}
	if !strings.Contains(string(gate), "selfhost integrity PASS: "+head+";") {
		return errors.New("gate log is not for publisher source")
	}
	for _, name := range []string{"server/internal/daemon\t", "server/internal/daemon/execenv\t", "server/internal/daemon/repocache\t", "server/pkg/agent\t"} {
		if !strings.Contains(string(gate), name) {
			return errors.New("full selfhost behavioral gate evidence required")
		}
	}
	if strings.Contains(string(gate), "FAIL") {
		return errors.New("selfhost gate failed")
	}
	if !filepath.IsAbs(packageDir) || !filepath.IsAbs(output) {
		return errors.New("absolute package and output paths required")
	}
	kinds := map[string]bool{}
	for _, artifact := range catalog.Artifacts {
		if !digestPattern.MatchString(artifact.SHA256) || !idPattern.MatchString(artifact.ID) {
			return errors.New("invalid immutable package name")
		}
		if artifact.Commit != head {
			return errors.New("all update units must bind the same source revision")
		}
		file, err := os.Open(filepath.Join(packageDir, artifact.SHA256))
		if err != nil {
			return err
		}
		err = VerifyArtifact(file, artifact)
		file.Close()
		if err != nil {
			return err
		}
		kinds[artifact.Kind] = true
	}
	if len(catalog.Artifacts) != 3 || len(kinds) != 3 || !kinds["server-web"] || !kinds["cli-daemon"] || !kinds["desktop"] {
		return errors.New("complete Mini and Desktop unit inventory required")
	}
	if err := verifyLocalFile(privateKey, "", true); err != nil {
		return err
	}
	raw, err := os.ReadFile(privateKey)
	if err != nil {
		return err
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(key) != ed25519.PrivateKeySize {
		return errors.New("invalid private channel signing key")
	}
	digest := sha256.Sum256(gate)
	catalog.Source = &SourceProof{Commit: head, Repository: ForkSourceURL, GateSHA256: hex.EncodeToString(digest[:]), Merged: !isolation, Isolation: isolation}
	payload, err := json.Marshal(catalog)
	if err != nil {
		return err
	}
	envelope, err := json.Marshal(Envelope{Payload: base64.StdEncoding.EncodeToString(payload), Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, payload))})
	if err != nil {
		return err
	}
	verified, err := VerifyCatalog(envelope, Policy{PublicKey: ed25519.PrivateKey(key).Public().(ed25519.PublicKey), Channel: catalog.Channel, Origin: origin, RequireSource: true})
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(output), 0700); err != nil {
		return err
	}
	unlock, err := lockJournal(output + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	cursor := output + ".sequence.json"
	var previous struct {
		Sequence uint64 `json:"sequence"`
		Digest   string `json:"digest"`
	}
	if data, readErr := os.ReadFile(cursor); readErr == nil {
		if err = decodeStrict(data, &previous); err != nil {
			return err
		}
	} else if !os.IsNotExist(readErr) {
		return readErr
	}
	if verified.Sequence <= previous.Sequence {
		return errors.New("publisher sequence replay or downgrade")
	}
	// Commit the immutable catalog first. A crash before the cursor write is
	// detected by the same payload's existing sequence before another publish.
	if data, readErr := os.ReadFile(output); readErr == nil {
		existing, err := VerifyCatalog(data, Policy{PublicKey: ed25519.PrivateKey(key).Public().(ed25519.PublicKey), Channel: catalog.Channel, Origin: origin, RequireSource: true})
		if err != nil {
			return err
		}
		if verified.Sequence <= existing.Sequence {
			return errors.New("published catalog sequence did not advance")
		}
	} else if !os.IsNotExist(readErr) {
		return readErr
	}
	if err = atomicPublish(output, envelope); err != nil {
		return err
	}
	data, _ := json.Marshal(map[string]any{"sequence": verified.Sequence, "digest": verified.Digest})
	return atomicPublish(cursor, data)
}
func atomicPublish(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".publish-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err = file.Write(data); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("publish candidate: %w", err)
	}
	return syncDirectory(filepath.Dir(path))
}

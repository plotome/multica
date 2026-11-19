// Package forkupdate implements the trust and transaction boundary for selfhost
// updates. Lifecycle adapters remain local policy, never executable feed data.
package forkupdate

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"regexp"
)

type Artifact struct {
	ID                string            `json:"id"`
	Host              string            `json:"host"`
	Kind              string            `json:"kind"`
	Platform          string            `json:"platform"`
	Arch              string            `json:"arch"`
	Version           string            `json:"version"`
	Commit            string            `json:"commit"`
	URL               string            `json:"url"`
	SHA256            string            `json:"sha256"`
	Bytes             int64             `json:"bytes"`
	ComponentVersions map[string]string `json:"component_versions"`
}

type Catalog struct {
	Source    *SourceProof `json:"source,omitempty"`
	Schema    int          `json:"schema"`
	Channel   string       `json:"channel"`
	Sequence  uint64       `json:"sequence"`
	Artifacts []Artifact   `json:"artifacts"`
}

// Payload is base64 rather than embedded JSON: signatures cover exact bytes,
// independent of JSON encoders, whitespace and property ordering.
type Envelope struct {
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

type Policy struct {
	RequireSource bool
	PublicKey     ed25519.PublicKey
	Channel       string
	Origin        string
}

type VerifiedCatalog struct {
	Catalog
	Digest string `json:"digest"`
}

var (
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	idPattern     = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
)

func decodeStrict(data []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("unexpected trailing JSON")
	}
	return nil
}

// VerifyCatalog never obtains trust keys or the allowed origin from the feed.
// Sequence is checked again against the durable receipt when planning.
func VerifyCatalog(data []byte, policy Policy) (VerifiedCatalog, error) {
	var out VerifiedCatalog
	var envelope Envelope
	if err := decodeStrict(data, &envelope); err != nil {
		return out, err
	}
	payload, err := base64.StdEncoding.DecodeString(envelope.Payload)
	if err != nil {
		return out, err
	}
	signature, err := base64.StdEncoding.DecodeString(envelope.Signature)
	if err != nil {
		return out, err
	}
	if len(policy.PublicKey) != ed25519.PublicKeySize || !ed25519.Verify(policy.PublicKey, payload, signature) {
		return out, errors.New("untrusted Fork catalog signature")
	}
	if err := decodeStrict(payload, &out.Catalog); err != nil {
		return out, err
	}
	origin, err := url.Parse(policy.Origin)
	if err != nil || !validUpdateOrigin(origin) || origin.Host == "" || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || (origin.Path != "" && origin.Path != "/") {
		return out, errors.New("policy requires HTTPS or private loopback artifact origin")
	}
	if policy.Channel == "" || out.Schema != 1 || out.Channel != policy.Channel || out.Sequence == 0 || out.Sequence > math.MaxInt64 || len(out.Artifacts) == 0 {
		return out, errors.New("wrong schema, channel or empty candidate")
	}
	seen := map[string]bool{}
	for _, a := range out.Artifacts {
		u, err := url.Parse(a.URL)
		if err != nil || u.Scheme != origin.Scheme || u.Host != origin.Host || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
			return out, fmt.Errorf("artifact %q is outside pinned origin", a.ID)
		}
		if !idPattern.MatchString(a.ID) || !idPattern.MatchString(a.Host) || seen[a.ID] || !commitPattern.MatchString(a.Commit) || !digestPattern.MatchString(a.SHA256) || a.Bytes <= 0 || a.Bytes == math.MaxInt64 || a.Version == "" {
			return out, fmt.Errorf("invalid or duplicate artifact %q", a.ID)
		}
		if (a.Kind != "desktop" && a.Kind != "server-web" && a.Kind != "cli-daemon") || (a.Platform != "darwin" && a.Platform != "linux" && a.Platform != "win32") || (a.Arch != "arm64" && a.Arch != "amd64") {
			return out, fmt.Errorf("unsupported artifact %q", a.ID)
		}
		seen[a.ID] = true
		expected := ExpectedComponents(a.Kind)
		if len(a.ComponentVersions) != len(expected) {
			return out, fmt.Errorf("artifact %q has incomplete component versions", a.ID)
		}
		for _, kind := range expected {
			if a.ComponentVersions[kind] == "" {
				return out, fmt.Errorf("artifact %q is missing version for %s", a.ID, kind)
			}
		}
	}
	if policy.RequireSource || out.Source != nil {
		proof := out.Source
		if proof == nil || proof.Repository != ForkSourceURL || !commitPattern.MatchString(proof.Commit) || !digestPattern.MatchString(proof.GateSHA256) || proof.Isolation != (out.Channel == "selfhost-test") || proof.Merged == proof.Isolation {
			return out, errors.New("trusted source and full gate proof required")
		}
		for _, artifact := range out.Artifacts {
			if artifact.Commit != proof.Commit {
				return out, errors.New("mixed source candidate inventory")
			}
		}
	}
	hash := sha256.Sum256(payload)
	out.Digest = hex.EncodeToString(hash[:])
	return out, nil
}

// VerifyArtifact bounds reads by the signed length; a URL or file name alone
// never establishes package identity. Transport must also reject redirects
// outside Policy.Origin before calling this function.
func VerifyArtifact(r io.Reader, artifact Artifact) error {
	if artifact.Bytes <= 0 || artifact.Bytes == math.MaxInt64 || !digestPattern.MatchString(artifact.SHA256) {
		return errors.New("invalid artifact length or digest")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(r, artifact.Bytes+1))
	if err != nil {
		return err
	}
	if n != artifact.Bytes || hex.EncodeToString(h.Sum(nil)) != artifact.SHA256 {
		return ErrArtifactMismatch
	}
	return nil
}

// Native selfhost profiles already use private loopback HTTP (including local
// tunnels). Remote artifact origins always require HTTPS. Both remain pinned,
// authenticated and redirect-free; a feed cannot add another origin.
func validUpdateOrigin(origin *url.URL) bool {
	return origin.Scheme == "https" || (origin.Scheme == "http" && (origin.Hostname() == "127.0.0.1" || origin.Hostname() == "localhost" || origin.Hostname() == "::1"))
}

var ErrArtifactMismatch = errors.New("artifact length or digest mismatch")

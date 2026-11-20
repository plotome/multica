package forkupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// AdapterPolicy is installed locally, never decoded from signed feed content.
// Pin the executable and script independently so an update URL cannot become
// a shell command, and a mutable adapter cannot silently change authority.
type AdapterPolicy struct {
	Executable       string `json:"executable"`
	ExecutableSHA256 string `json:"executable_sha256"`
	Script           string `json:"script"`
	ScriptSHA256     string `json:"script_sha256"`
	Config           string `json:"config"`
}

func verifyLocalFile(path, digest string, private bool) error {
	if !filepath.IsAbs(path) {
		return errors.New("local update policy requires absolute paths")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || private && info.Mode().Perm()&0077 != 0 {
		return errors.New("unsafe local update policy file")
	}
	if digest != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		if !digestPattern.MatchString(digest) || hex.EncodeToString(sum[:]) != digest {
			return errors.New("local update tool checksum changed")
		}
	}
	return nil
}

type releaseAdapter struct {
	policy      AdapterPolicy
	target      Target
	packagePath string
}

func (p AdapterPolicy) Factory(ctx context.Context, target Target, path string) (Driver, error) {
	for _, asset := range []struct{ path, sum string }{{p.Executable, p.ExecutableSHA256}, {p.Script, p.ScriptSHA256}} {
		if err := verifyLocalFile(asset.path, asset.sum, false); err != nil {
			return nil, err
		}
	}
	if err := verifyLocalFile(p.Config, "", true); err != nil {
		return nil, err
	}
	adapter := &releaseAdapter{policy: p, target: target, packagePath: path}
	if err := adapter.call(ctx, "prepare", nil); err != nil {
		return nil, err
	}
	return adapter, nil
}
func (d *releaseAdapter) call(ctx context.Context, action string, result any) error {
	request, err := json.Marshal(map[string]any{"target": d.target, "package_path": d.packagePath})
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, d.policy.Executable, d.policy.Script, "--config", d.policy.Config, "--action", action)
	if err := inheritHostLock(ctx, cmd); err != nil {
		return err
	}
	cmd.Stdin = bytes.NewReader(request)
	var stdout boundedAdapterOutput
	cmd.Stdout = &stdout
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Native adapter errors are structured; arbitrary subprocess stderr stays
		// local and is not posted as a potentially credential-bearing API message.
		var failure struct {
			Error    string `json:"error,omitempty"`
			Deferred string `json:"deferred,omitempty"`
		}
		if decodeStrict(stdout.Bytes(), &failure) == nil {
			if failure.Deferred != "" {
				return adapterDeferredError{errors.New(failure.Deferred)}
			}
			if failure.Error != "" {
				return fmt.Errorf("release adapter %s: %s", action, failure.Error)
			}
		}
		return adapterInterruptedError{fmt.Errorf("release adapter %s interrupted: %w", action, err)}
	}
	if result == nil {
		var ack struct{}
		return decodeStrict(stdout.Bytes(), &ack)
	}
	return decodeStrict(stdout.Bytes(), result)
}
func (d *releaseAdapter) Probe(ctx context.Context, t Target) (Probe, error) {
	d.target = t
	var p Probe
	err := d.call(ctx, "probe", &p)
	return p, err
}
func (d *releaseAdapter) PrepareRecovery(ctx context.Context, t Target) (RecoveryPlan, error) {
	d.target = t
	var p RecoveryPlan
	err := d.call(ctx, "recovery", &p)
	return p, err
}
func (d *releaseAdapter) Install(ctx context.Context, t Target) error {
	d.target = t
	return d.call(ctx, "install", nil)
}
func (d *releaseAdapter) Verify(ctx context.Context, t Target) (Observation, error) {
	d.target = t
	var o Observation
	err := d.call(ctx, "verify", &o)
	return o, err
}
func (d *releaseAdapter) Rollback(ctx context.Context, t Target) error {
	d.target = t
	return d.call(ctx, "rollback", nil)
}
func (d *releaseAdapter) VerifyRollback(ctx context.Context, t Target) (Observation, error) {
	d.target = t
	var o Observation
	err := d.call(ctx, "verify-rollback", &o)
	return o, err
}

type adapterInterruptedError struct{ error }
type hostLockContextKey struct{}

// The adapter protocol contains metadata, never bulk package bytes.
type boundedAdapterOutput struct{ bytes.Buffer }

func (b *boundedAdapterOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 1024*1024 {
		return 0, errors.New("adapter protocol output exceeds one MiB")
	}
	return b.Buffer.Write(p)
}

type adapterDeferredError struct{ error }

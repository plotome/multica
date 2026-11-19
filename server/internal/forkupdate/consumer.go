package forkupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
)

type ControlClient struct {
	BaseURL, RuntimeID, Token string
	HTTP                      *http.Client
}

func (c ControlClient) request(ctx context.Context, method, path string, body any, result any) error {
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return err
	}
	if c.HTTP == nil || c.Token == "" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return errors.New("invalid private update control client")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1")) {
		return errors.New("remote update control requires HTTPS")
	}
	var data []byte
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	client := *c.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return errControlRedirect }
	response, err := client.Do(req)
	if err != nil {
		if errors.Is(err, errControlRedirect) || ctx.Err() != nil {
			return err
		}
		return controlUnavailableError{err}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		err := fmt.Errorf("update control returned HTTP %d", response.StatusCode)
		if response.StatusCode >= 500 || response.StatusCode == http.StatusTooManyRequests {
			return controlUnavailableError{err}
		}
		return err
	}
	if result == nil {
		_, err = io.Copy(io.Discard, response.Body)
		return err
	}
	return json.NewDecoder(response.Body).Decode(result)
}

func (c ControlClient) Fetch(ctx context.Context) (ControlState, error) {
	var state ControlState
	err := c.request(ctx, http.MethodGet, "/api/daemon/runtimes/"+url.PathEscape(c.RuntimeID)+"/fork-update", nil, &state)
	return state, err
}

func (c ControlClient) Report(ctx context.Context, state ControlState, id string, result Target) error {
	return c.request(ctx, http.MethodPost, "/api/daemon/runtimes/"+url.PathEscape(c.RuntimeID)+"/fork-update/report", map[string]any{"revision": state.Revision, "target": id, "result": result}, nil)
}

type DriverFactory func(context.Context, Target, string) (Driver, error)

// Consumer is called by a process outside the daemon/app lifecycle. No bearer
// token, command path or local installation policy comes from the catalog.
type Consumer struct {
	Client                            ControlClient
	Policy                            Policy
	Host, StateDir, LockDir, CacheDir string
	Artifacts                         *http.Client
	Driver                            DriverFactory
}

// Tick performs one foreground reconciliation. Supervisors schedule subsequent
// calls; there is no in-memory-only approval and no daemon-owned subprocess.
func (c Consumer) Tick(ctx context.Context) error {
	state, err := c.Client.Fetch(ctx)
	if err != nil {
		var unavailable controlUnavailableError
		if !errors.As(err, &unavailable) {
			return err
		}
		state, recoveryErr := c.recoverControlState()
		if recoveryErr != nil {
			return errors.Join(err, recoveryErr)
		}
		return c.reconcile(ctx, state, true)
	}
	return c.reconcile(ctx, state, false)
}

func (c Consumer) reconcile(ctx context.Context, state ControlState, offline bool) error {
	if len(state.Envelope) == 0 {
		return nil
	}
	candidate, err := VerifyCatalog(state.Envelope, c.Policy)
	if err != nil {
		return err
	}
	if candidate.Digest != state.Receipt.Digest || candidate.Sequence != state.Receipt.Sequence || len(candidate.Artifacts) != len(state.Receipt.Targets) {
		return errors.New("control plan does not match signed candidate")
	}
	for _, artifact := range candidate.Artifacts {
		target, ok := state.Receipt.Targets[artifact.ID]
		if !ok || !reflect.DeepEqual(artifact, target.Artifact) {
			return errors.New("control plan altered signed artifact")
		}
	}
	if !filepath.IsAbs(c.StateDir) || !filepath.IsAbs(c.LockDir) || !idPattern.MatchString(c.Host) || c.Driver == nil {
		return errors.New("invalid local consumer policy")
	}
	j := Journal{Path: filepath.Join(c.StateDir, candidate.Digest+".json"), LockDir: c.LockDir}
	if _, err := os.Lstat(j.Path); os.IsNotExist(err) {
		installed := map[string]Baseline{}
		for id, target := range state.Receipt.Targets {
			installed[id] = target.Baseline
		}
		plan, err := Plan(candidate, installed)
		if err != nil {
			return err
		}
		if err := j.Create(plan); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err := j.withLock(func() error {
		local, err := j.load()
		if err != nil {
			return err
		}
		if local.Digest != state.Receipt.Digest {
			return errors.New("local candidate changed")
		}
		for id, remote := range state.Receipt.Targets {
			if remote.Artifact.Host != c.Host {
				continue
			}
			previous := local.Targets[id]
			if remote.Attempt < previous.Attempt {
				return errors.New("control operation replay")
			}
			if remote.Attempt > previous.Attempt {
				local.Targets[id] = remote
			}
		}
		local.Approved = state.Receipt.Approved
		return j.save(local)
	}); err != nil {
		return err
	}
	if !offline {
		if err := c.cacheControlState(state); err != nil {
			return err
		}
	}
	var deliveryErr error
	report := func(id string, target Target) {
		current, err := c.Client.Fetch(ctx)
		if err == nil && current.Receipt.Digest != candidate.Digest {
			err = errors.New("active candidate changed during installation")
		}
		if err == nil && !reflect.DeepEqual(current.Receipt.Targets[id], target) {
			err = c.Client.Report(ctx, current, id, target)
		}
		if err != nil {
			deliveryErr = err
		} else {
			deliveryErr = nil
		}
	}
	for _, artifact := range candidate.Artifacts {
		if artifact.Host != c.Host {
			continue
		}
		local, err := j.Read()
		if err != nil {
			return err
		}
		target := local.Targets[artifact.ID]
		j.OnCheckpoint = func(receipt Receipt) { report(artifact.ID, receipt.Targets[artifact.ID]) }
		if target.Phase == Failed {
			report(artifact.ID, target)
			return deliveryErr
		}
		if target.Phase == Succeeded || target.Phase == RolledBack {
			report(artifact.ID, target)
			continue
		}
		checkpoint := func(phase Phase, stage string, cause error, probe *Probe) error {
			return j.withLock(func() error {
				r, err := j.load()
				if err != nil {
					return err
				}
				t := r.Targets[artifact.ID]
				if cause != nil {
					t.ResumePhase = t.Phase
					if !r.Approved || t.Phase == Downloading || t.Phase == Validating {
						t.ResumePhase = Prepared
					}
					t.FailureStage = stage
					t.Error = cause.Error()
					t.Log = cause.Error()
					t.Phase = Failed
				} else {
					t.Phase = phase
				}
				if probe != nil {
					t.RollbackAvailable = probe.RollbackAvailable
					t.Observation = probe.Observation
				}
				r.Targets[artifact.ID] = t
				return j.save(r)
			})
		}
		path := ""
		if target.Operation == "install" && !target.PackageReady {
			preparing := target.Phase == Prepared || target.Phase == Downloading || target.Phase == Validating
			if preparing {
				if err := checkpoint(Downloading, "", nil, nil); err != nil {
					return err
				}
			}
			path, err = DownloadArtifact(ctx, c.Artifacts, c.Policy, artifact, c.CacheDir)
			if err != nil {
				saveErr := checkpoint(Failed, "download", err, nil)
				return errors.Join(err, saveErr, deliveryErr)
			}
			if preparing {
				if err := checkpoint(Validating, "", nil, nil); err != nil {
					return err
				}
			}
		}
		if target.PackageReady {
			path = filepath.Join(c.CacheDir, artifact.SHA256)
		}
		if target.PackageReady && !state.Receipt.Approved {
			report(artifact.ID, target)
			continue
		}
		driver, err := c.Driver(ctx, target, path)
		if err != nil {
			saveErr := checkpoint(Failed, "prepare", err, nil)
			return errors.Join(err, saveErr, deliveryErr)
		}
		if target.Operation == "install" && !target.PackageReady {
			if err := j.withLock(func() error {
				r, err := j.load()
				if err != nil {
					return err
				}
				t := r.Targets[artifact.ID]
				t.PackageReady = true
				r.Targets[artifact.ID] = t
				return j.save(r)
			}); err != nil {
				return err
			}
		}
		if !state.Receipt.Approved {
			probe, err := driver.Probe(ctx, target)
			if err != nil {
				saveErr := checkpoint(Failed, "probe", err, nil)
				return errors.Join(err, saveErr, deliveryErr)
			}
			phase := Prepared
			if !probe.Online || probe.Busy {
				phase = Waiting
			}
			if err := checkpoint(phase, "", nil, &probe); err != nil {
				return err
			}
			continue
		}
		if target.Phase == Prepared || target.Phase == Downloading || target.Phase == Validating {
			if err := checkpoint(Prepared, "", nil, nil); err != nil {
				return err
			}
		}
		if target.Operation == "install" {
			driver = verifiedPackageDriver{Driver: driver, path: path}
		}
		if err := j.Advance(ctx, artifact.ID, c.Host, driver); err != nil {
			return errors.Join(err, deliveryErr)
		}
	}
	return deliveryErr
}

package forkupdate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// A control outage may not strand an already started, durably approved local
// installation. Cached approval cannot start a new install while disconnected.
func (c Consumer) recoverControlState() (ControlState, error) {
	var state ControlState
	if !filepath.IsAbs(c.StateDir) {
		return state, errors.New("absolute consumer state directory required")
	}
	data, err := os.ReadFile(filepath.Join(c.StateDir, "active-control.json"))
	if err != nil {
		return state, err
	}
	if err = decodeStrict(data, &state); err != nil {
		return state, err
	}
	if !digestPattern.MatchString(state.Receipt.Digest) {
		return state, errors.New("invalid cached control digest")
	}
	receipt, err := (Journal{Path: filepath.Join(c.StateDir, state.Receipt.Digest+".json"), LockDir: c.LockDir}).Read()
	if err != nil {
		return state, err
	}
	if !receipt.Approved {
		return state, errors.New("offline preparation cannot start an installation")
	}
	for _, target := range receipt.Targets {
		if target.Artifact.Host != c.Host {
			continue
		}
		switch target.Phase {
		case Restoring, VerifyingRestore, Installing, Verifying, RollingBack, VerifyingRollback, Succeeded, RolledBack, Failed:
		default:
			return state, errors.New("offline approval cannot start a new local operation")
		}
	}
	state.Receipt = receipt
	return state, nil
}
func (c Consumer) cacheControlState(state ControlState) error {
	if !filepath.IsAbs(c.StateDir) {
		return errors.New("absolute consumer state directory required")
	}
	f, err := os.CreateTemp(c.StateDir, ".control-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = json.NewEncoder(f).Encode(state); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(c.StateDir, "active-control.json")); err != nil {
		return err
	}
	return syncDirectory(c.StateDir)
}

type controlUnavailableError struct{ error }

var errControlRedirect = errors.New("control endpoint redirect refused")

package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"

	"github.com/google/uuid"
)

// The profile process lock admits one writer. A durable lease keeps a KeepAlive
// relaunch from claiming tasks between updater preparation and service removal.
type updateLease struct {
	ID     string `json:"id"`
	Digest string `json:"digest"`
}
type updateMaintenanceRequest struct {
	updateLease
	PID    int    `json:"pid"`
	Commit string `json:"commit"`
}

var updateRevision = regexp.MustCompile(`^[0-9a-f]{40}$`)
var updateDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (d *Daemon) updateLeasePath() string {
	return filepath.Join(d.cfg.MaintenanceDir, "update-lease.json")
}
func (d *Daemon) loadUpdateMaintenance() {
	if d.cfg.MaintenanceDir == "" {
		return
	}
	lease, err := d.readUpdateLease()
	if err != nil || lease != nil {
		d.updateMaintenance = true
		if err != nil {
			d.logger.Error("update maintenance lease unreadable; task claims paused", "error", err)
		}
	}
}
func (d *Daemon) readUpdateLease() (*updateLease, error) {
	data, err := os.ReadFile(d.updateLeasePath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var lease updateLease
	if err = json.Unmarshal(data, &lease); err != nil {
		return nil, err
	}
	if _, err = uuid.Parse(lease.ID); err != nil || !updateDigest.MatchString(lease.Digest) {
		return nil, errors.New("invalid persisted update lease")
	}
	return &lease, nil
}
func (d *Daemon) writeUpdateLease(lease updateLease) error {
	if err := os.MkdirAll(d.cfg.MaintenanceDir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(d.cfg.MaintenanceDir, ".update-lease-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = json.NewEncoder(f).Encode(lease); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), d.updateLeasePath()); err != nil {
		return err
	}
	return syncUpdateMaintenanceDir(d.cfg.MaintenanceDir)
}
func syncUpdateMaintenanceDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (d *Daemon) updateMaintenanceHandler(resume bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		if !filepath.IsAbs(d.cfg.MaintenanceDir) {
			http.Error(w, "update maintenance unavailable", 503)
			return
		}
		var req updateMaintenanceRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			http.Error(w, "invalid update request", 400)
			return
		}
		if _, err := uuid.Parse(req.ID); err != nil || !updateDigest.MatchString(req.Digest) {
			http.Error(w, "invalid update lease", 400)
			return
		}
		if req.PID != os.Getpid() || !updateRevision.MatchString(req.Commit) || req.Commit != d.cfg.CLICommit {
			http.Error(w, "daemon identity changed or unverified", 409)
			return
		}
		d.claimMu.Lock()
		defer d.claimMu.Unlock()
		lease, err := d.readUpdateLease()
		if err != nil {
			d.updateMaintenance = true
			http.Error(w, "persisted lease unreadable", 503)
			return
		}
		if lease != nil && *lease != req.updateLease {
			http.Error(w, "another update holds maintenance", 409)
			return
		}
		if resume {
			if lease == nil {
				http.Error(w, "update lease not held", 409)
				return
			}
			if err = os.Remove(d.updateLeasePath()); err == nil {
				err = syncUpdateMaintenanceDir(d.cfg.MaintenanceDir)
			}
			if err != nil {
				http.Error(w, "could not persist update resume", 503)
				return
			}
			d.updateMaintenance = false
		} else {
			if lease == nil {
				if d.pauseClaims || d.claimsInFlight > 0 || d.activeTasks.Load() > 0 {
					http.Error(w, "daemon is busy", 409)
					return
				}
				// Set the memory gate even when fsync fails after rename. Releasing a gate
				// whose durability is uncertain could start work during a service switch.
				d.updateMaintenance = true
				if err = d.writeUpdateLease(req.updateLease); err != nil {
					http.Error(w, "could not persist update lease", 503)
					return
				}
			}
			d.updateMaintenance = true
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"status":"ok"}`)
	}
}

// Stop only the process holding this already-persisted lease. No signal/force
// fallback exists; a KeepAlive successor remains paused by the same lease.
func (d *Daemon) updateMaintenanceStopHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var req updateMaintenanceRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			http.Error(w, "invalid update request", 400)
			return
		}
		d.claimMu.Lock()
		defer d.claimMu.Unlock()
		lease, err := d.readUpdateLease()
		if err != nil || lease == nil || *lease != req.updateLease || !d.updateMaintenance || req.PID != os.Getpid() || !updateRevision.MatchString(req.Commit) || req.Commit != d.cfg.CLICommit {
			http.Error(w, "verified update lease is not held by this process", 409)
			return
		}
		if d.cancelFunc == nil {
			http.Error(w, "shutdown unavailable", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"status":"ok"}`)
		go d.cancelFunc()
	}
}

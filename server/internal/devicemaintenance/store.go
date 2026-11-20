// Package devicemaintenance is the shared device fence for identity maintenance
// and coordinated upgrades. It never cancels tasks or treats heartbeat expiry
// as proof that a daemon has finished its local preparation/cleanup.
package devicemaintenance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/util"
)

type Database interface {
	Begin(context.Context) (pgx.Tx, error)
}
type Store struct{ DB Database }
type Request struct {
	Lease      string `json:"lease"`
	Digest     string `json:"digest"`
	Generation string `json:"generation"`
}
type State struct {
	Protocol      int      `json:"protocol"`
	DatabaseFence bool     `json:"database_fence"`
	DaemonID      string   `json:"daemon_id"`
	Lease         string   `json:"lease,omitempty"`
	Digest        string   `json:"digest,omitempty"`
	Generation    string   `json:"generation,omitempty"`
	Frozen        bool     `json:"frozen"`
	ServerDrained bool     `json:"server_drained"`
	LocalDrained  bool     `json:"local_drained"` // Always false: local proof belongs to the executor.
	Pending       int64    `json:"pending"`
	Workspaces    []string `json:"workspaces"`
}
type Error struct {
	Code    int
	Message string
}

func (e *Error) Error() string               { return e.Message }
func refusal(code int, message string) error { return &Error{code, message} }

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// Act authenticates the complete device scope under the same exclusive lock
// used by registration/task triggers. Any failure rolls back the entire scope.
func (s Store) Act(ctx context.Context, device, actor, action string, req Request) (out State, err error) {
	if _, e := util.ParseUUID(device); e != nil {
		return out, refusal(400, "invalid daemon identity")
	}
	if _, e := util.ParseUUID(actor); e != nil {
		return out, refusal(403, "human identity required")
	}
	switch action {
	case "capabilities", "acquire", "status", "drain", "release":
	default:
		return out, refusal(404, "unknown device maintenance action")
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	// Assert the actual database fence, not just a backend version string.
	var version, guards int
	err = tx.QueryRow(ctx, `SELECT device_maintenance_version(),
        (SELECT count(*) FROM pg_trigger WHERE
          ((tgrelid='agent_task_queue'::regclass AND tgname='device_maintenance_task')
           OR (tgrelid='agent_runtime'::regclass AND tgname='device_maintenance_runtime'))
          AND tgenabled IN ('O','A') AND NOT tgisinternal)`).Scan(&version, &guards)
	if err != nil || version != 1 || guards != 2 {
		return out, refusal(503, "device maintenance database fence unavailable")
	}
	if _, err = tx.Exec(ctx, `SELECT device_maintenance_lock($1,true)`, device); err != nil {
		return out, err
	}
	out = State{Protocol: 1, DatabaseFence: true, DaemonID: device, Workspaces: []string{}}
	rows, err := tx.Query(ctx, `SELECT DISTINCT r.workspace_id::text, r.owner_id::text,
        EXISTS(SELECT 1 FROM member m WHERE m.workspace_id=r.workspace_id AND m.user_id=$2::uuid
               AND (r.owner_id=$2::uuid OR m.role IN ('owner','admin')))
        FROM agent_runtime r WHERE r.daemon_id=$1`, device, actor)
	if err != nil {
		return out, err
	}
	owner := ""
	scope := map[string]bool{}
	permitted := true
	for rows.Next() {
		var ws string
		var own *string
		var allowed bool
		if err = rows.Scan(&ws, &own, &allowed); err != nil {
			rows.Close()
			return out, err
		}
		if own == nil || !allowed {
			permitted = false
		} else if owner == "" {
			owner = *own
		} else if owner != *own {
			permitted = false
		}
		if !scope[ws] {
			scope[ws] = true
			out.Workspaces = append(out.Workspaces, ws)
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return out, err
	}
	if len(scope) == 0 {
		return out, refusal(404, "device not found")
	}
	if !permitted {
		return State{}, refusal(403, "management authority for the complete device is required")
	}
	// Keep membership/role decisions stable until this all-scope transaction
	// commits. Recheck after waiting for a concurrent membership mutation.
	memberships, lockErr := tx.Query(ctx, `SELECT workspace_id::text,role FROM member
        WHERE workspace_id=ANY($1::uuid[]) AND user_id=$2::uuid ORDER BY workspace_id FOR SHARE`, out.Workspaces, actor)
	if lockErr != nil {
		return out, lockErr
	}
	verified := 0
	for memberships.Next() {
		var ws, role string
		if err = memberships.Scan(&ws, &role); err != nil {
			memberships.Close()
			return out, err
		}
		if actor != owner && role != "owner" && role != "admin" {
			memberships.Close()
			return out, refusal(403, "device scope permission changed")
		}
		verified++
	}
	memberships.Close()
	if err = memberships.Err(); err != nil {
		return out, err
	}
	if verified != len(scope) {
		return out, refusal(403, "device scope permission changed")
	}
	var storedOwner string
	err = tx.QueryRow(ctx, `SELECT lease::text,digest,generation::text,server_drained,device_owner::text
        FROM device_maintenance WHERE daemon_id=$1`, device).Scan(&out.Lease, &out.Digest, &out.Generation, &out.ServerDrained, &storedOwner)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	out.Frozen = err == nil
	if !out.Frozen && (action == "status" || action == "capabilities") {
		// Reconcile a lost release acknowledgement against its durable binding.
		e := tx.QueryRow(ctx, `SELECT body->'state'->>'lease',body->'state'->>'digest',body->'state'->>'generation'
		 FROM device_maintenance_event WHERE daemon_id=$1 AND action='release'
		 AND body->>'device_owner'=$2 ORDER BY id DESC LIMIT 1`, device, owner).Scan(&out.Lease, &out.Digest, &out.Generation)
		if e != nil && !errors.Is(e, pgx.ErrNoRows) {
			return out, e
		}
	}
	if out.Frozen && storedOwner != owner {
		return out, refusal(409, "device ownership changed while frozen")
	}
	if action == "acquire" {
		if _, e := util.ParseUUID(req.Lease); e != nil || !digestPattern.MatchString(req.Digest) {
			return out, refusal(400, "lease UUID and digest required")
		}
		if out.Frozen {
			if out.Lease != req.Lease || out.Digest != req.Digest {
				return out, refusal(409, "another maintenance lease holds the device")
			}
		} else {
			out.Lease = req.Lease
			out.Digest = req.Digest
			out.Generation = uuid.NewString()
			_, err = tx.Exec(ctx, `INSERT INTO device_maintenance(daemon_id,lease,digest,generation,device_owner,actor_id,workspace_ids,admitted_tasks)
                SELECT $1,$2::uuid,$3,$4::uuid,$5::uuid,$6::uuid,$7::uuid[],COALESCE(array_agg(t.id),ARRAY[]::uuid[])
                FROM agent_task_queue t JOIN agent_runtime r ON r.id=t.runtime_id
                WHERE r.daemon_id=$1 AND (t.status IN ('dispatched','waiting_local_directory','running')
                  OR (t.status NOT IN ('queued','deferred') AND t.completed_at IS NULL)
                  OR t.failure_reason='cleanup_unconfirmed' OR t.result->>'process_cleanup_pending'='true')`,
				device, out.Lease, out.Digest, out.Generation, owner, actor, out.Workspaces)
			if err != nil {
				return out, err
			}
			out.Frozen = true
		}
	}
	if action == "drain" || action == "release" {
		if !out.Frozen || req.Lease != out.Lease || req.Digest != out.Digest || req.Generation != out.Generation {
			return out, refusal(409, "maintenance lease/generation differs")
		}
	}
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue t JOIN agent_runtime r ON r.id=t.runtime_id
        WHERE r.daemon_id=$1 AND (t.status IN ('dispatched','waiting_local_directory','running')
          OR (t.status NOT IN ('queued','deferred') AND t.completed_at IS NULL)
          OR t.failure_reason='cleanup_unconfirmed' OR t.result->>'process_cleanup_pending'='true')`, device).Scan(&out.Pending); err != nil {
		return out, err
	}
	if action == "drain" {
		if out.Pending != 0 {
			return out, refusal(409, "existing task preparation/execution/cleanup is not settled")
		}
		_, err = tx.Exec(ctx, `UPDATE device_maintenance SET server_drained=true WHERE daemon_id=$1`, device)
		if err != nil {
			return out, err
		}
		out.ServerDrained = true
	}
	if action == "release" {
		if !out.ServerDrained || out.Pending != 0 {
			return out, refusal(409, "server drain proof required before release")
		}
		_, err = tx.Exec(ctx, `DELETE FROM device_maintenance WHERE daemon_id=$1`, device)
		if err != nil {
			return out, err
		}
		out.Frozen = false
	}
	if action == "acquire" || action == "drain" || action == "release" {
		record, _ := json.Marshal(struct {
			State       State  `json:"state"`
			Actor       string `json:"actor"`
			DeviceOwner string `json:"device_owner"`
		}{out, actor, owner})
		if _, err = tx.Exec(ctx, `INSERT INTO device_maintenance_event(daemon_id,action,body) VALUES($1,$2,$3::jsonb)`, device, action, record); err != nil {
			return out, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return out, err
	}
	return out, nil
}

// PublicError deliberately excludes SQL, task contents and credentials.
func PublicError(err error) (int, []byte) {
	var e *Error
	if !errors.As(err, &e) {
		e = &Error{503, "device maintenance unavailable; retain the current freeze"}
	}
	b, _ := json.Marshal(map[string]string{"error": e.Message})
	return e.Code, b
}
func (s State) String() string {
	return fmt.Sprintf("device maintenance protocol %d frozen=%t server_drained=%t local_drained=false", s.Protocol, s.Frozen, s.ServerDrained)
}

# Device maintenance protocol 1

This is source and isolated-test preparation, not a deployed capability or a
release candidate. The database fence is shared by first identity contact and
coordinated upgrades. It does not replace the updater's full-revision, approval,
local maintenance-lease, compatibility, or consumer gates.

## Server contract

Human-authenticated GET ` /api/device-maintenance/{daemonId}/capabilities` and
`status`, POST `acquire`, `drain`, `release`. Mutation bodies contain `lease`
(UUID), `digest` (SHA256), and the server-issued `generation` for drain/release.
The CLI is `multica device maintenance <action> <daemon-id> --output json` with
the corresponding binding flags. Mutations refuse daemon-task contexts.

Acquire resolves **every** runtime/workspace for that daemon under the device
transaction lock. All registrations must have one attested account owner, and
the actor must be a member with owner/admin or device-owner authority in every
workspace. Membership rows remain locked through commit; any error rolls back
the complete acquire. No partial freeze, timeout release, or task cancellation.

Migration 643 enforces the device gate at PostgreSQL task/runtime write
boundaries. Registration validates the actual final authenticated runtime owner,
including upsert ownership preservation; a client-supplied matching daemon ID is
insufficient. New providers inherit the fence. New workspaces require the lease
actor's authority before being included. Liveness/heartbeat cannot release it.

Already dispatched/preparing/running/cleanup tasks are recorded under the gate.
They may finish and report progress/results/cleanup; no new claim generation is
allowed. Drain rejects outstanding dispatched, waiting, running, unsettled
terminal, or cleanup-unconfirmed rows. It returns **server_drained only**;
`local_drained` is always false. Missing local evidence is never zero.

Status/release are bound to the persisted lease and generation. Events retain
acquire/drain/release history so a lost release acknowledgement can be reconciled
without a second switch or an unsafe recovery after ingress has opened.

## Deployment and backend rollback boundary

The normal full self-host upgrade also installs a CLI/restarts a daemon: it is
not an approved way to bootstrap this capability while preserving old bytes.
Prepare a reviewed **backend-only** deployment: preserve the exact old backend
image ID, configuration and volumes, backup and verify the database, and inspect
the complete migration range before touching production. This source alone is
not a production deployment package; unmerged builds are isolation artifacts.

The new backend image's existing `docker/entrypoint.sh` runs migrations before
the API starts. After the separately approved backend-only rollout, a fresh
authenticated operator CLI must verify protocol 1 and `database_fence=true`, the
target scope and the expected running backend image/source. Health alone is not
readiness for device maintenance. Until then no owner acquire/identity switch.

On deployment failure, no acquire is allowed. On API rollback, retain schema
643 and its task/runtime triggers: an old backend cannot silently dispatch past
them, even though it lacks the lease API (claims fail explicitly). Restore a
compatible backend to inspect/release a frozen device. Do not rewrite statuses,
unbind runtimes, disable triggers or restore a database snapshot to clear it.

Migration 643 down first locks the maintenance table, then refuses if **any**
device lease exists. This closes acquire-vs-down races. Removing the schema is
not part of backend-image rollback. Downgrading to an image that cannot operate
with the expanded schema must remain stopped until a compatible image is ready;
availability loss is preferable to reopening claims without a fence.

## Reproduction

`make test-device-maintenance` uses only this checkout's existing loopback
worktree database, applies migrations and runs focused race-enabled real
transactions and HTTP/WS routes. It never creates/starts a database container.

`make test-device-maintenance-consumer consumer_python=<absolute Python>
consumer_script=<absolute consumer fixture>` runs the Python executor against
the same HTTP handler and PostgreSQL store. The explicit integration build tag
adds that test to the normal transaction tests; it requires both paths and fails
if either is absent. Native App/launchd operations are substitutes, never real
host actions. Keep both repository SHAs and the fixture's hashes with results.

## Remaining local protocol limitation

The first-contact consumer requires a locally attested drain schema including
in-flight claims, preparation, cleanup, active/running tasks and pending/failed
terminal reports. Existing legacy status responses without this proof are
rejected **before acquire or service stop**. The isolated consumer fixture
supplies a simulated complete local proof; it does not validate an old daemon's
missing protocol. A real first contact remains blocked until that local evidence
gap is resolved without changing the approved original CLI bytes or weakening
the gate. Identity acceptance uses the exact accepted App's real launchd storage
probe plus the actual daemon process chain; it remains `upgrade_eligible=false`.

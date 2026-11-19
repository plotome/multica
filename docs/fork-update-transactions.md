# Fork update transaction core

The `fork-update` CLI currently verifies signed candidates, prepares an
unapproved local operation receipt, and inspects it. It does **not** install,
publish, enable a feed, or replace the packaged Desktop manual-update guard.
The install API is exercised with isolated disk fixtures, not live hosts.

## Trust contract

A locally pinned Ed25519 public key, exact channel and HTTPS artifact origin
are required. A feed cannot supply its own trust root. The envelope contains
base64 `payload` and `signature`; the signature covers the decoded payload bytes.
Payload schema 1 carries a monotonically increasing `sequence` and the complete
artifact inventory. Each artifact binds its component ID, host, kind, platform,
architecture, version, full source commit, URL, byte length and SHA-256.

An installed inventory is required for **every** target. Planning rejects a
missing component, a replay/older sequence, a host or platform mismatch. The
inventory must ultimately come from the lifecycle adapter's actual observation;
the preparatory CLI accepts an explicit operator-provided inventory file.
Signed sequence ordering does not itself prove source ancestry. The publisher
must retain the existing selfhost integration and merged-source gates.

`VerifyArtifact` validates already-opened bytes. `DownloadArtifact` enforces the
pinned HTTPS origin, refuses redirects and exposes only verified cache entries.
Release adapters must also verify before extraction and reuse existing archive,
codesign and provenance checks.
No signing key or private deployment information belongs in this repository.

## Local commands

Run `multica fork-update <verify|plan|status> --help` for flags. `verify` and
`plan` require an explicit catalog file, pinned public key file, channel and
origin. `plan` additionally requires the installed inventory and a new receipt
path. `status` reads that receipt and includes a computed `complete` boolean.
There is deliberately no CLI install command at this stage.

## Durable lifecycle boundary

The journal lives outside the application bundle, daemon worktree and temporary
directory. It is a host-local technical transaction receipt, not a distributed
queue or business datastore. Writes use fsync plus atomic replacement; an OS
file lock excludes concurrent installers and releases on process death.

Approval binds the exact catalog payload digest, never `latest`. Offline and
busy states retain approval without a TTL. Each target independently traverses
prepared → waiting → installing → verifying → succeeded. Installation is
checkpointed before side effects. A restarted worker reconciles installation
through the existing idempotent release receipt; a verifying checkpoint does
not reinstall. Failures remain visible and require an explicit retry. Rollback
has its own durable direction, including after rollback failure. Unknown states,
empty inventories, partial success and rolled-back targets cannot be complete.

`Driver` is the integration boundary for existing release tools. It must:

- Probe actual host availability and busy state. Transport failure must not be
  treated as success or an idle machine.
- Acquire the existing lifecycle/claim gate immediately before stopping work;
  this package's preliminary busy probe does not close the probe/stop race.
- Install and roll back idempotently using the established package transactions.
- Verify actual running source identity, preserved profile/workspace/daemon
  identity, configuration and data, not merely files or successful process spawn.
- Run under a durable process supervisor outside the daemon/app it replaces.
  Never clear task environment variables to bypass managed-task protection.

## Integration checkpoint

The proposed UI extends Desktop Settings → Updates with one candidate action
and per-component results. Existing checks/preparation and native update entry
points should be reused. The short-lived runtime CLI update store is unsuitable
as the durable cross-host approval ledger: it expires pending requests after
120 seconds and retains requests for only five minutes.

The second checkpoint below adds the native authenticated PostgreSQL ledger,
consumer library, artifact downloader and renderer settings panel. Still required
before end-to-end acceptance: a supervised host-local consumer, automatic private
catalog publication/checking, concrete release-tool adapters, preload/main
lifecycle wiring, source/publisher gates, and real consumer verification. These are
implementation work, not tasks for an operator to execute by hand.

The packaged manual guard and its accepted manifest capability remain in effect
until those integrations pass. At that cutover, replace the guard, manual-mode
copy and `manual-desktop-maintenance` acceptance together with trusted-channel
and coordinated-upgrade tests. Removing only the guard or manifest entry is not
a valid migration. No current operation receipt claims a deployed upgrade.

## Implemented second checkpoint (not an installable candidate)

`Artifact.component_versions` now enumerates every expected component of an
update unit. An observation must include a timestamp, nonempty actual version
and the expected full source revision for every running component. Desktop
contains Desktop/CLI/daemon; server-web contains server/web; cli-daemon contains
CLI/daemon. A failed or rolled-back unit never counts as overall success.

The PostgreSQL `fork_update_rollout` ledger stores the unchanged signed envelope,
receipt and revision. Workspace advisory locking serializes sequence creation;
revision compare-and-swap and per-target attempt fencing reject stale commands
and results. The private host policy binds one owner and explicit runtime IDs.
User actions use existing native user authentication and membership; machine
actors cannot approve. Daemon requests use native runtime authorization plus the
policy binding. The policy is an absolute, non-symlink, non-writable-by-others
local file selected with `MULTICA_FORK_UPDATE_CONFIG`; feed data cannot configure
it. It is not enabled by default.

`Consumer.Tick` automatically downloads from the pinned HTTPS origin without
redirects and only exposes an atomic cache entry after length and digest checks.
Preparation never approves installation. The install boundary revalidates a
prepared cache. A control outage does not interrupt a previously started local
operation, including worker restart; cached approval cannot begin a new offline
installation. Results remain in the journal for later delivery. This is a library
integration, not an installed service: no production driver or supervisor exists
yet. Its network and PostgreSQL tests use fixture authentication and disk
installations, not production package switches.

Failed operations retain `operation`, `failure_stage`, and `resume_phase`.
Retry restores that checkpoint and increments the attempt; it never turns a
rollback into installation. Rollback availability must be explicitly attested by
the lifecycle adapter. Rollback verification has a separate durable checkpoint.
The host lock directory is explicit local policy shared across all transaction
files, closing the different-receipt concurrency bypass.

The daemon adds a profile-local durable maintenance lease. `daemon
prepare-update --lease <uuid> --digest <sha256> --expected-commit <full-sha>`
atomically refuses busy tasks/in-flight claims, persists the lease and pauses
new claims without stopping the process. A KeepAlive relaunch retains the pause.
`daemon resume-update` requires the same lease and verified process identity.
Corrupt state pauses claims and reports an error; it is not silently deleted.
A transient barrier release cannot remove the durable gate. Both commands keep
the existing managed-task prohibition. The release-tool adapter must obtain this
gate, verify its exact managed service PID, and keep the lease until all host
units have passed runtime verification. The simpler `stop --when-idle` API alone
does not protect a host whose launchd job has KeepAlive enabled.

Desktop's existing manual-mode settings tab now mounts the coordinated update
panel through the native authenticated API client and TanStack Query. It shows
actual observations separately from target component versions, target-specific
retry/rollback, failure stages and logs. This does not enable packaged app
installation. Automatic notifications/polling, main/preload lifecycle handoff,
private catalog publishing, real release adapters and an external supervised
consumer are still required. The existing manual-update guard remains until
those integrations and their replacement acceptance checks pass together.

One deployment choice remains open: when the Mini backend is itself the
approval API, an unsuccessful backend switch also removes the remote retry UI.
The final adapter must either restore a proven, schema-compatible backend
baseline locally before reporting failure, or preserve a separately supervised
control endpoint. A database-wide restore is not an automatic fallback; it
requires the repository's explicit production-data authorization. No production
controller/proxy, daemon or app was switched by this checkpoint.

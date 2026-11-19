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

`VerifyArtifact` validates already-opened bytes. It is not a downloader: the
transport integration must constrain redirects to the pinned origin, verify
before extraction and reuse the existing archive, codesign and provenance checks.
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

Still required before end-to-end acceptance: authenticated durable cross-host
approval/result transport, a supervised host-local consumer, automatic private
catalog check/download, concrete release-tool adapters, renderer/preload/main
wiring, source/publisher gates, and real consumer verification. These are
implementation work, not tasks for an operator to execute by hand.

The packaged manual guard and its accepted manifest capability remain in effect
until those integrations pass. At that cutover, replace the guard, manual-mode
copy and `manual-desktop-maintenance` acceptance together with trusted-channel
and coordinated-upgrade tests. Removing only the guard or manifest entry is not
a valid migration. No current operation receipt claims a deployed upgrade.

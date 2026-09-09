# Codex conversation state

New Codex conversations on macOS/Linux use a dedicated `CODEX_HOME` per
profile, workspace, agent and issue/chat, with separate generations for fresh
sessions. Execution roots keep their existing Prepare/Reuse lifecycle;
subprocesses and task credentials are refreshed per run.
The daemon holds the conversation's OS lock before configuration preparation
and until provider process cleanup and transcript collection finish. A second
concurrent execution is rejected with a setup error, not silently redirected to
another home. Windows retains task-local homes until process-tree cleanup has
equivalent confirmation.

## Ownership and compatibility

Codex owns its state files and database migrations. Multica does not enumerate,
copy, merge or reset its SQLite databases. Configuration, sandbox policy, shell
environment allowlist and bound skills are refreshed from current inputs before
each process starts. Configuration failure prevents launch. Shell snapshots
are execution artifacts and are cleared before a new run; native auto-memory
remains disabled by the existing policy. Authentication and plugin-cache links
retain their existing scope. Unchanged managed skill files retain identity and
timestamps; Codex retains ownership of `skills/.system`.

The state root is `CODEX_HOME/multica-homes-v1` under the daemon's shared Codex
home, not under task scratch. Its own home is then passed explicitly to the
provider. Profile names and composite identities are hashed or namespaced;
mutable titles and Task IDs do not change an enrolled resume's path. A fresh
run without a prior thread uses its Task ID to select a new generation. The
server's existing explicit session-reset operation therefore cannot inherit
another generation's hidden state. Runs without a stable workspace, agent and
conversation identity retain task-local state.

A durable thread binding is written before publishing an in-flight resume
pointer and again at terminal delivery. Missing/corrupt state for a known
binding fails closed. A persisted resume requires its original workdir and
rollout to remain available; the provider must return the requested thread ID.
It cannot silently fall back to `thread/start`, nor can a catalog-refresh retry
discard the resumed thread inside the same home.

These directories isolate normal executions, not malicious processes sharing
the same OS account. They do not make user-provided skills immutable. Codex
upgrades still require compatibility tests; a stable directory does not imply
database downgrade compatibility or guarantee that Codex stops injecting skill
catalogs. Measure native rollout deltas before claiming token/caching savings.

## Rollout and recovery

This is the new-conversation stage. Pre-upgrade thread IDs without a home
binding stay on the existing session-store path. There is no automatic search
through old task homes, live SQLite copy, bulk migration, or opt-in that can
mistake partial state for a complete migration. To use the new behavior for an
old conversation, explicitly start a fresh session; this does not restore lost
Goals or other state. Full old-home migration is not implemented by this stage.

The legacy path may be retired only after remaining old conversations have
ended/reset or a separately verified cold migration has enrolled them. Review
that cutover by 2026-10-09; do not call the overall old-state migration complete
until the old path is actually retired. No running Home is migrated during
deployment, and this change does not restart the daemon or publish a release.

`active.json` outside the provider home marks an execution until orderly
cleanup. If the daemon crashes or provider cleanup cannot be confirmed, it
remains and both execution and GC refuse to take over. An OS lock disappearing
alone is not proof that orphaned children stopped writing. Recovery requires an
operator to identify the exact conversation directory from the diagnostic,
verify that the old daemon and provider process tree no longer write it, acquire
the conversation lock, and retain a cold backup before clearing that one
marker. Do not delete `lease.lock`: deleting its inode can create two owners.
There is deliberately no automatic stale-PID recovery based only on age or PID
reuse. Restoring after a Codex downgrade likewise requires a matching cold
snapshot; never combine database main files with unrelated WAL files.

The existing Codex-session TTL also prunes inactive new Home generations. GC
takes the same OS lock and skips quarantined conversations. It leaves small
thread-binding tombstones and lock files so missing state is distinguishable
from an un-enrolled legacy session. Task-root GC cannot delete these homes, and
the new homes do not reference the old separately-pruned session stores. Cold
backups must include the bindings and the complete relevant generation, not
just `sessions/`. No current external backup policy is modified by this patch.

## Verification boundary

Default tests use generated fixtures, fake JSON-RPC providers and real
preparation subprocesses. They cover two-run process/credential separation,
Home continuity, opaque future-state preservation, skill updates/no-op refresh,
strict resume, cancellation cleanup evidence, generation reset, isolation,
symlink rejection and GC. They do not prove native non-empty Goal/queue
semantics or skill-prompt deduplication. Those require an explicitly authorized
real-provider smoke after deployment, with no production queue replay.

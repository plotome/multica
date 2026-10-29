# Runtime application approval

The Codex backend forwards native computer-use application elicitation to the
human who owns the run's device. This adds no authority to the agent or its task
token. It does not change command execution, file, or sandbox approval policy.

Native application requests no longer wait for a human on a live connection.
The daemon makes one bounded server request. A matching, unexpired owner grant
is atomically consumed; otherwise it replies `cancel`, never a saved `decline`.
The original request, warning and offered scopes remain in the task approval
record after the task finishes. The run transcript in Web and Desktop can still
answer that pending record.

The agent receives a JSON `computer_use_approval_required` notice through the
Codex active-turn steering channel, containing issue/task/request IDs, app,
original request and expiry. This is separate from the MCP cancellation reply:
MCP elicitation only accepts its protocol-defined response shapes. The agent
uses the existing owner decision card and wait ledger, then ends its turn. It
must not poll, retry the tool or substitute another app. Expired and denied
records produce `computer_use_approval_expired` / `computer_use_approval_denied`.
Already approved native apps never reach this path.

Only native empty-object forms marked `mcp_tool_call` / `computer-use` are
supported. Other elicitation shapes still fail closed; this does not intercept
upstream command, sandbox or JavaScript review. Session/always persistence is
returned only when the current native request offers that scope. `once` has no
persist metadata; session and always each consume one pending grant and let the
runtime enforce its chosen persistence. Grants cannot cross issue, runtime,
agent, app or workspace. A lost response consumes the grant without a retry.

`COMPUTER_USE_APPROVAL_TTL` is a required positive Go duration, with no implicit
default. It bounds both the card deadline and the pending grant, starting when
the request is recorded. Missing configuration fails closed. Migration 596
adds expiry/consumption fields and invalidates old live pending requests; legacy
records cannot turn into deferred grants. Issue/workspace deletion removes the
records; task termination only removes legacy live-connection records.

Human endpoints keep the machine-credential rejection (`task_token` and
`cloud_pat`) and device-owner check. The existing decision-card service may
relay an authenticated owner answer using a signed receipt bound to decision,
owner, issue, task, request, choice and deadline. Configure its exact member ID
in `COMPUTER_USE_CARD_SERVICE_USER_ID` and the card service's existing signing
secret in `COMPUTER_USE_CARD_SECRET`; unset values disable this relay. The
service must verify its owner callback before signing, and commit/verify the
grant before the existing card reply/wakeup proceeds. This is a trusted service
bridge, not a claim of isolation from another process running as the same OS
user. Do not put either secret in the Codex runtime config.

`multica application-approval get TASK REQUEST` lets that configured service
reconcile owner answers made in Multica. `respond ... --choice CHOICE
--card-receipt-file FILE` relays a receipt through the same CLI authentication.
No task token can write a grant. The card service uses its existing work queue
to mirror native answers, post the existing card reply, and wake the declared
wait exactly once. Refusal and timeout wake only to report the operation cannot
proceed. The configuration and service deployment must accompany this backend
and daemon change; fixtures do not prove Air desktop acceptance.

## Approval-surface isolation

Requests to control Multica Desktop, Obsidian, common Web browsers, terminals
and dedicated agent hosts are rejected before presenting choices. This prevents
native CUA from clicking its own approval in those apps, including when a Web
browser is not an ancestor of the daemon. Keep LCU's native host guard enabled;
never set `LCU_ALLOW_AGENT_HOST_APPROVAL=1`.

For this candidate, review approvals only in Multica Desktop or a protected
Chrome/Safari/Firefox/Edge/Brave/Vivaldi/Opera/Arc browser. Do not approve a new
unlisted browser or app containing approval controls. Browser-connector and
audio approval modes are unsupported and fail closed. Existing external grants
must be reviewed before activation. This is not OS-level isolation against an
agent with independent shell/accessibility automation; actual consumer testing
must establish the intended input boundary. A separate approval device is the
stronger deployment option when the execution environment can control the
operator's desktop independently.

## Host Codex runtime config

A locally installed Codex runtime extension (for example a computer-use MCP
server) can create environment variables after the daemon snapshot and launch
its kernel through `codex sandbox`, whose task shell policy would drop them.
The owner describes what that runtime needs in a host-local JSON file, read by
the daemon for every Codex task (no restart after regeneration):

- Path: `~/.multica/codex-runtime.json`, or `MULTICA_CODEX_RUNTIME_CONFIG` in the
  daemon environment. Absent file means off.
- The file must be a regular file (not a symlink) owned by the daemon user, and
  neither it nor its directory may be group- or world-writable. It must live
  outside the workspaces root. Any invalid or untrusted file fails the task;
  it never degrades to launching without the runtime.

```json
{
  "schema": 1,
  "id": "runtime-and-version-provenance",
  "agent_ids": ["optional agent UUIDs; omit for every Codex agent"],
  "include_only": ["LATE_RUNTIME_NAME"],
  "set": {"FIXED_BOOTSTRAP_NAME": "owner-reviewed value"},
  "task_home_mcp_servers": ["mcp-server-name"]
}
```

`include_only` adds exact names whose values the runtime itself produces. `set`
fixes values; its names are also kept by the filter (Codex applies `set` before
`include_only`), and an agent `custom_env` entry with the same name fails the
task instead of overriding it. `task_home_mcp_servers` rebinds the named MCP
registrations copied from the owner's Codex config to the task `CODEX_HOME` and
drops owner tool `approval_mode`; an inline registration fails because it cannot
be isolated. Names must be uppercase and exact. Credentials, wildcards,
Multica/Codex/loader/shell controls, `NODE_OPTIONS`, `NODE_PATH`, request
metadata and anything naming sandbox, permission or approval are rejected. The
untrusted Node worker allowlist may appear only in `set`, never as a pass-through.

Multica code carries no runtime names or versions. Generate the file from the
installed runtime, record its provenance in `id`, and validate a real consumer
in a fresh task after each runtime update.

## Deployment and rollback

Build the server, CLI/daemon, shared Web and Desktop from the same reviewed
commit. Apply additive migrations 593–596 before starting the new server. No
existing hot table changes shape. Existing installed clients remain compatible
but cannot show approval choices: use the matching Web UI or upgrade Desktop.
A new daemon against an old server cancels approval rather than granting it.

Install and trust the original LCU native adapter and lifecycle hooks using its
supported setup flow in the intended configuration scope; inspect the task's
copied hooks/trust and runtime config before enabling the pilot. This change alone
does not register LCU. Validate each real consumer in a new session, including
refuse, cancel, stop, background action, result readback and uninterrupted user
input. Fixture tests are not desktop acceptance.

For rollback, stop accepting new pilot runs, cancel/finish existing runs, remove
the host Codex runtime config, then restore the previous daemon/server/UI versions.
Keep the additive tables during application rollback; they are not read by old
versions. A later schema rollback may remove 596, 595, 594, then 593 after confirming
no new server remains. Do not restart the daemon that hosts the upgrade task.
Local candidate packages are unsigned/ad-hoc and not public releases; remote
push, PR, production promotion and runtime activation require the separate
owner authorization applicable to that environment.

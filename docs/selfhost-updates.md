# Fork update contract

This fork has an integration line, not a collection of independently deployed
feature branches. A commit id is build provenance, not the owner's update UI.

## Sources of truth

- `upstream/main`: upstream input, never a push target.
- `origin/main`: the only selfhost integration line and source of deployment candidates.
- `selfhost/patches.json`: accepted local capabilities which every candidate must retain.
- `Selfhost integration integrity`: ancestry, regression-test presence, and focused behavioral checks.

The persistent physical worktree patch was previously deployed from a feature
branch but omitted from the integration line. Compiling that line did not detect
the lost capability. The manifest explicitly retains that original commit;
copying its files without preserving ancestry is not the repair.

## Update procedure

1. Read the deployed build identity and lifecycle configuration. Do not assume
   the installed build is the current main branch. Obtain the commit from build
   metadata, not from a human remembering it.
2. Start a `selfhost/*` branch from `origin/main`. Merge upstream and every
   accepted local patch with non-rewriting merges. Do not deploy feature branches,
   rebase shared history, or omit a patch because an upstream update builds.
3. Run `python3 scripts/check-selfhost-patches.py --current <deployed-commit>`.
   Any deployed commit missing from the candidate blocks promotion. Add the
   missing history through a merge; do not weaken the check to hide the omission.
   The manifest also detects omission after a bad intermediate build has already
   replaced the previously good installation.
4. Run the focused integration workflow locally and the broader tests warranted
   by the change. Verify default configuration, not only an opt-in test setup.
   Record baseline failures explicitly; do not call a failing suite green.
5. Open the fork PR with the retained/changed/retired capability list and test
   evidence. Merge normally with the reviewed head pinned; do not bypass branch
   protection. Verify the merged tree matches the tested tree and rerun the
   integrity check against the merged commit.
6. Prepare the owner handoff: a human-readable candidate label (PR title/number),
   machine-resolved immutable build identity, retained capabilities, test result,
   and the host's existing atomic-upgrade command. Do not create public tags or
   Releases unless separately authorized. Never choose `latest` at execution time.
7. The owner runs the atomic upgrader outside daemon tasks. It owns backup,
   build/artifact checks, service/CLI switching and restart. The agent must not
   restart the daemon hosting its own task.
8. After the owner reports restart complete, the agent verifies the running build
   and performs the two-turn real smoke below. Only then mark delivery complete.
   If it fails, retain the evidence and keep the integration issue open.

## Two-turn acceptance

Use one agent and issue with `local_directory` in worktree mode. Finish the first
run before starting the second. Check:

- Both runs read and write the issue using their own task credentials.
- Task IDs and provider process PIDs differ; credentials are never printed.
- The physical worktree and Codex session IDs remain the same.
- A random nonsecret marker created only in the first turn's tool output is
  recovered from resumed context and hashed again in the second turn. Do not
  retrieve the marker through external history, files, or issue comments.
- A different agent is isolated by the persistent worktree identity; the focused
  tests cover this without spending another inference run.

The first run after migrating a disposable worktree may start fresh. Establish
the two-turn baseline after the upgrade; do not claim history from an already
lost disposable session was restored.

Persistent worktrees are enabled by default in this fork. An explicit
`MULTICA_LOCAL_WORKTREE_LIFECYCLE=task` disables them and cannot satisfy this
acceptance; report such an override before deployment rather than silently
changing the host's configuration.

## Retiring or replacing a patch

Keep the old commit in history. Use an explicit revert or replacement commit,
explain the decision in the same PR, and update the manifest and behavioral
acceptance together. An upstream equivalent must pass the same acceptance before
the local implementation is removed. Manifest edits are contract changes, not a
way to make a red build green.

## Remaining automation boundary

This repository provides the integration contract and CI gate, not the host's
deployment controller. The existing host updater still accepts an explicit
commit. A complete no-SHA owner interface requires that controller to consume a
reviewed candidate record, resolve and pin its identity once, enforce this gate,
and retain the prior deployment receipt. Until that is implemented, the operator
must generate the exact existing upgrade command; the owner is not asked to
select or compare commit ids. Do not represent this runbook as an automatic
promotion/deployment gate or as already enforced branch protection.

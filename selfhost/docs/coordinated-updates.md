# Private coordinated Fork updates

The private channel uses the existing authenticated API, PostgreSQL rollout
ledger, native CLI profile and native release tools. It never uses the official
updater feed. A catalog binds three immutable units to one full source commit.
The publisher requires clean source, full selfhost gate evidence and merge
membership. Unmerged source is limited to `selfhost-test` isolation packages.
HTTPS is required remotely; an explicitly pinned loopback HTTP origin supports
the existing selfhost topology. Package requests use each host's own native
credential; credentials are neither catalog data nor transferable artifacts.

A separately supervised worker owns a private local journal and pinned Python
release adapter. Desktop and the daemon do not supervise their own installer.
The worker's CLI remains outside the app being replaced. A host-wide advisory
lock follows adapter children across worker death. The daemon marks that lock
close-on-exec before starting its long-lived background process.

Before service pause, the journal saves approval, candidate digest, baseline
identity, recovery assets, bidirectional compatibility evidence and direction.
An unverified baseline or unknown database compatibility leaves the unit in
preparation. Database restore, table rebuild and database-container recreation
are absent from the adapter. Only backend/frontend code containers are switched.

Candidate failure durably changes direction to baseline recovery. Recovery can
resume while the API is down and after worker restart. Execution/verification
failure is sticky; an explicit retry resumes baseline recovery, not candidate
installation. A restored baseline remains a failed update. A later explicit
candidate retry starts another attempt. Reports retain actual running component
versions separately from candidate failure and baseline recovery results.

First installation of the independent supervisor is a separate bootstrap step.
A managed task may only provision an explicit isolated test namespace. Existing
manual maintenance remains available until the production bootstrap and full
packaged acceptance have been completed; this source change alone does not
complete that migration or authorize a production switch.

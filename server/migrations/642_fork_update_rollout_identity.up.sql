CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS fork_update_rollout_identity
    ON fork_update_rollout (workspace_id, digest);

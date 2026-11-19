CREATE TABLE IF NOT EXISTS fork_update_rollout (
    workspace_id uuid NOT NULL,
    digest text NOT NULL,
    sequence bigint NOT NULL CHECK (sequence > 0),
    body jsonb NOT NULL,
    revision bigint NOT NULL CHECK (revision > 0)
);

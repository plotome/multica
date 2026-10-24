CREATE TABLE task_elicitation (
 id UUID NOT NULL,
 task_id UUID NOT NULL,
 workspace_id UUID NOT NULL,
 request JSONB NOT NULL,
 response JSONB,
 status TEXT NOT NULL CHECK (status IN ('pending','responded','cancelled')),
 created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

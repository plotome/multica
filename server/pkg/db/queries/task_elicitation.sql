-- name: CreateTaskElicitation :one
WITH live AS MATERIALIZED (
 SELECT t.id FROM agent_task_queue t JOIN agent_runtime r ON r.id=t.runtime_id
 WHERE t.id= @task_id AND r.workspace_id= @workspace_id AND t.status='running' FOR UPDATE OF t
)
INSERT INTO task_elicitation(id,task_id,workspace_id,request,status,expires_at)
SELECT @id,live.id,@workspace_id,@request,'pending',@expires_at FROM live RETURNING *;

-- name: GetTaskElicitation :one
SELECT e.* FROM task_elicitation e JOIN agent_task_queue t ON t.id=e.task_id
WHERE e.id= @id AND e.task_id= @task_id AND e.workspace_id= @workspace_id;

-- name: ListTaskElicitations :many
SELECT e.* FROM task_elicitation e JOIN agent_task_queue t ON t.id=e.task_id
WHERE e.task_id= @task_id AND e.workspace_id= @workspace_id AND e.status='pending' AND e.expires_at>now()
ORDER BY e.created_at;

-- name: RespondTaskElicitation :one
WITH live AS MATERIALIZED (
 SELECT t.id FROM agent_task_queue t JOIN agent_runtime r ON r.id=t.runtime_id
 WHERE t.id= @task_id AND r.workspace_id= @workspace_id AND r.owner_id= @owner_id FOR UPDATE OF t
)
UPDATE task_elicitation e SET response= @response,status='responded' FROM live
WHERE e.id= @id AND e.task_id=live.id AND e.workspace_id= @workspace_id AND e.status='pending' AND e.expires_at>now() RETURNING e.*;

-- name: CancelTaskElicitation :exec
UPDATE task_elicitation SET status='cancelled',response=NULL
WHERE id= @id AND task_id= @task_id AND workspace_id= @workspace_id AND status='pending';

-- name: ConsumeTaskElicitation :one
WITH candidate AS MATERIALIZED (
 SELECT e.id FROM task_elicitation e
 JOIN agent_task_queue original ON original.id=e.task_id
 JOIN agent_task_queue current ON current.id= @task_id
 WHERE e.workspace_id= @workspace_id AND current.status='running'
 AND original.issue_id=current.issue_id AND current.issue_id IS NOT NULL
 AND original.runtime_id=current.runtime_id
 AND original.agent_id=current.agent_id
 AND e.request->'_meta'->'tool_params'->>'app'= @app::text
 AND e.status='responded' AND e.response->>'action'='accept'
 AND e.expires_at>now() AND e.consumed_by IS NULL
 AND (e.response->'_meta'->>'persist' IS NULL OR @offered_scopes::jsonb ? (e.response->'_meta'->>'persist'))
 ORDER BY e.created_at DESC LIMIT 1 FOR UPDATE OF e SKIP LOCKED
)
UPDATE task_elicitation e SET consumed_by= @task_id,consumed_at=now()
FROM candidate WHERE e.id=candidate.id RETURNING e.*;

-- name: FindUnconsumedTaskElicitation :one
SELECT e.* FROM task_elicitation e
JOIN agent_task_queue original ON original.id=e.task_id
JOIN agent_task_queue current ON current.id= @task_id
WHERE e.workspace_id= @workspace_id AND original.issue_id=current.issue_id
AND current.issue_id IS NOT NULL AND original.runtime_id=current.runtime_id
AND original.agent_id=current.agent_id
AND e.request->'_meta'->'tool_params'->>'app'= @app::text
AND e.consumed_by IS NULL AND e.expires_at IS NOT NULL
ORDER BY e.created_at DESC LIMIT 1;

-- name: DeleteLiveTaskElicitations :exec
DELETE FROM task_elicitation WHERE task_id=ANY(@task_ids::uuid[]) AND expires_at IS NULL;

-- name: DeleteTaskElicitations :exec
DELETE FROM task_elicitation WHERE task_id= ANY(@task_ids::uuid[]);

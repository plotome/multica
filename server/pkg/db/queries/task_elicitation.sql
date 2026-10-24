-- name: CreateTaskElicitation :one
WITH live AS MATERIALIZED (
 SELECT t.id FROM agent_task_queue t JOIN agent_runtime r ON r.id=t.runtime_id
 WHERE t.id= @task_id AND r.workspace_id= @workspace_id AND t.status='running' FOR UPDATE OF t
)
INSERT INTO task_elicitation(id,task_id,workspace_id,request,status)
SELECT @id,live.id,@workspace_id,@request,'pending' FROM live RETURNING *;

-- name: GetTaskElicitation :one
SELECT e.* FROM task_elicitation e JOIN agent_task_queue t ON t.id=e.task_id
WHERE e.id= @id AND e.task_id= @task_id AND e.workspace_id= @workspace_id AND t.status='running';

-- name: ListTaskElicitations :many
SELECT e.* FROM task_elicitation e JOIN agent_task_queue t ON t.id=e.task_id
WHERE e.task_id= @task_id AND e.workspace_id= @workspace_id AND e.status='pending' AND t.status='running'
ORDER BY e.created_at;

-- name: RespondTaskElicitation :one
WITH live AS MATERIALIZED (
 SELECT t.id FROM agent_task_queue t JOIN agent_runtime r ON r.id=t.runtime_id
 WHERE t.id= @task_id AND r.workspace_id= @workspace_id AND r.owner_id= @owner_id AND t.status='running' FOR UPDATE OF t
)
UPDATE task_elicitation e SET response= @response,status='responded' FROM live
WHERE e.id= @id AND e.task_id=live.id AND e.workspace_id= @workspace_id AND e.status='pending' RETURNING e.*;

-- name: CancelTaskElicitation :exec
UPDATE task_elicitation SET status='cancelled',response=NULL
WHERE id= @id AND task_id= @task_id AND workspace_id= @workspace_id;

-- name: DeleteTaskElicitations :exec
DELETE FROM task_elicitation WHERE task_id= ANY(@task_ids::uuid[]);

ALTER TABLE task_elicitation
 ADD COLUMN expires_at TIMESTAMPTZ,
 ADD COLUMN consumed_by UUID,
 ADD COLUMN consumed_at TIMESTAMPTZ;

-- Live-connection approvals cannot be promoted into deferred grants.
UPDATE task_elicitation SET status='cancelled',response=NULL WHERE status='pending';

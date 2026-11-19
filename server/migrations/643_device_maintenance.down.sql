-- Serialize the guard with acquire itself; a pre-DDL SELECT alone races a
-- lease committing between that SELECT and DROP TRIGGER.
LOCK TABLE device_maintenance IN ACCESS EXCLUSIVE MODE;
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM device_maintenance) THEN
        RAISE EXCEPTION 'release device maintenance before removing its database fence';
    END IF;
END $$;
DROP TRIGGER device_maintenance_task ON agent_task_queue;
DROP TRIGGER device_maintenance_runtime ON agent_runtime;
DROP FUNCTION device_maintenance_task_guard();
DROP FUNCTION device_maintenance_runtime_guard();
DROP FUNCTION device_maintenance_version();
DROP FUNCTION device_maintenance_lock(text, boolean);
DROP TABLE device_maintenance;
DROP TABLE device_maintenance_event;

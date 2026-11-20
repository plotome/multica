-- The database fence deliberately also covers an older backend's SQL. A
-- backend rollback must not turn a durable maintenance receipt into a bypass.
CREATE TABLE device_maintenance (
    daemon_id text NOT NULL,
    lease uuid NOT NULL,
    digest text NOT NULL CHECK (digest ~ '^[0-9a-f]{64}$'),
    generation uuid NOT NULL,
    device_owner uuid NOT NULL,
    actor_id uuid NOT NULL,
    workspace_ids uuid[] NOT NULL,
    admitted_tasks uuid[] NOT NULL,
    server_drained boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE device_maintenance_event (
    id bigserial NOT NULL,
    daemon_id text NOT NULL,
    action text NOT NULL,
    body jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE FUNCTION device_maintenance_lock(device text, exclusive boolean) RETURNS void
LANGUAGE plpgsql AS $$
BEGIN
    IF exclusive THEN
        PERFORM pg_advisory_xact_lock(hashtextextended('device-maintenance:' || device, 0));
    ELSE
        PERFORM pg_advisory_xact_lock_shared(hashtextextended('device-maintenance:' || device, 0));
    END IF;
END $$;

CREATE FUNCTION device_maintenance_runtime_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE gate device_maintenance; device text;
BEGIN
    -- Include updates (heartbeats/re-registration) and teardown/rebind. Device
    -- identity is immutable while frozen, so moving a row cannot evade a fence.
    device := CASE WHEN TG_OP = 'DELETE' THEN OLD.daemon_id ELSE NEW.daemon_id END;
    IF TG_OP = 'UPDATE' AND OLD.daemon_id IS DISTINCT FROM NEW.daemon_id THEN
        RAISE EXCEPTION 'runtime daemon identity is immutable' USING ERRCODE = '55000';
    END IF;
    IF device IS NULL THEN RETURN COALESCE(NEW, OLD); END IF;
    PERFORM device_maintenance_lock(device, false);
    SELECT * INTO gate FROM device_maintenance WHERE daemon_id = device;
    IF FOUND THEN
        IF TG_OP = 'DELETE' THEN
            RAISE EXCEPTION 'device maintenance prevents runtime deletion' USING ERRCODE = '55000';
        END IF;
        IF NEW.owner_id IS DISTINCT FROM gate.device_owner OR NOT EXISTS (
            SELECT 1 FROM member m WHERE m.workspace_id = NEW.workspace_id
              AND m.user_id = gate.actor_id
              AND (gate.actor_id = gate.device_owner OR m.role IN ('owner','admin'))
        ) THEN
            RAISE EXCEPTION 'frozen device identity or workspace authority differs' USING ERRCODE = '42501';
        END IF;
        IF TG_OP = 'UPDATE' AND OLD.workspace_id IS DISTINCT FROM NEW.workspace_id THEN
            RAISE EXCEPTION 'frozen runtime workspace is immutable' USING ERRCODE = '55000';
        END IF;
        UPDATE device_maintenance SET workspace_ids = ARRAY(
            SELECT DISTINCT x FROM unnest(workspace_ids || ARRAY[NEW.workspace_id]) x
        ) WHERE daemon_id = device AND NOT NEW.workspace_id = ANY(workspace_ids);
    END IF;
    RETURN COALESCE(NEW, OLD);
END $$;
-- AFTER observes the final owner after an upsert's COALESCE, including legacy
-- daemon-token registration. A rejected registration rolls back atomically.
CREATE TRIGGER device_maintenance_runtime
AFTER INSERT OR UPDATE OR DELETE ON agent_runtime
FOR EACH ROW EXECUTE FUNCTION device_maintenance_runtime_guard();

CREATE FUNCTION device_maintenance_task_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE device text; gate device_maintenance;
BEGIN
    IF TG_OP = 'UPDATE' AND NEW.runtime_id IS DISTINCT FROM OLD.runtime_id THEN
        SELECT daemon_id INTO device FROM agent_runtime WHERE id = OLD.runtime_id;
        IF device IS NOT NULL THEN
            PERFORM device_maintenance_lock(device, false);
            IF EXISTS (SELECT 1 FROM device_maintenance WHERE daemon_id = device) THEN
                RAISE EXCEPTION 'frozen task runtime cannot be rebound' USING ERRCODE = '55000';
            END IF;
        END IF;
    END IF;
    SELECT daemon_id INTO device FROM agent_runtime WHERE id = NEW.runtime_id;
    IF device IS NULL THEN RETURN NEW; END IF;
    -- Only transitions that can admit work take the gate. Progress, result,
    -- terminal and process-cleanup reports remain available while frozen.
    IF NEW.status NOT IN ('dispatched','waiting_local_directory','running') THEN RETURN NEW; END IF;
    PERFORM device_maintenance_lock(device, false);
    SELECT * INTO gate FROM device_maintenance WHERE daemon_id = device;
    IF FOUND THEN
        IF TG_OP = 'INSERT' OR gate.server_drained OR NOT NEW.id = ANY(gate.admitted_tasks) THEN
            RAISE EXCEPTION 'device maintenance blocks task admission' USING ERRCODE = '55000';
        END IF;
        -- Reclaim must not mint another generation of an admitted claim.
        IF NEW.dispatched_at IS DISTINCT FROM OLD.dispatched_at THEN
            RAISE EXCEPTION 'device maintenance blocks task reclaim' USING ERRCODE = '55000';
        END IF;
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER device_maintenance_task
BEFORE INSERT OR UPDATE ON agent_task_queue
FOR EACH ROW EXECUTE FUNCTION device_maintenance_task_guard();

CREATE FUNCTION device_maintenance_version() RETURNS integer LANGUAGE sql IMMUTABLE AS $$ SELECT 1 $$;

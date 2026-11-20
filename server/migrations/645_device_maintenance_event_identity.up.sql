CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS device_maintenance_event_identity
    ON device_maintenance_event (id);

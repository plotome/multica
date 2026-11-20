CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS device_maintenance_identity
    ON device_maintenance (daemon_id);

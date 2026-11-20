package main

import (
	"context"
	"strings"
	"testing"
)

func TestDeviceMaintenanceFreshDDLAndRollbackGuard(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	var database string
	if e := pool.QueryRow(ctx, `SELECT current_database()`).Scan(&database); e != nil || !strings.HasPrefix(database, "multica_multica_") {
		t.Fatal("isolated worktree database required")
	}
	schema := createScratchSchema(t, ctx, pool, "device_maintenance_ddl_")
	scoped := openTestPoolWithSearchPath(t, schema)
	for _, sql := range []string{
		`CREATE TABLE agent_runtime(id uuid,daemon_id text,workspace_id uuid,owner_id uuid)`,
		`CREATE TABLE member(workspace_id uuid,user_id uuid,role text)`,
		`CREATE TABLE agent_task_queue(id uuid,runtime_id uuid,status text,dispatched_at timestamptz)`,
	} {
		if _, e := scoped.Exec(ctx, sql); e != nil {
			t.Fatal(e)
		}
	}
	versions := []string{"643_device_maintenance", "644_device_maintenance_identity", "645_device_maintenance_event_identity"}
	options := runOptions{Direction: "up", Files: realMigrationFiles(t, versions, "up"), SchemaMigrationsTable: schema + ".schema_migrations", Hooks: hooksForDirection("up")}
	if e := runMigrations(ctx, scoped, options); e != nil {
		t.Fatal(e)
	}
	assertIndexValidity(t, scoped, schema, "device_maintenance_identity", true)
	assertIndexValidity(t, scoped, schema, "device_maintenance_event_identity", true)
	var foreignKeys int
	if e := scoped.QueryRow(ctx, `SELECT count(*) FROM pg_constraint c JOIN pg_namespace n ON c.connamespace=n.oid WHERE n.nspname=$1 AND c.contype='f'`, schema).Scan(&foreignKeys); e != nil || foreignKeys != 0 {
		t.Fatal("new foreign keys", foreignKeys, e)
	}
	insert := `INSERT INTO device_maintenance(daemon_id,lease,digest,generation,device_owner,actor_id,workspace_ids,admitted_tasks) VALUES('fixture',md5('lease')::uuid,repeat('a',64),md5('generation')::uuid,md5('owner')::uuid,md5('owner')::uuid,'{}','{}')`
	if _, e := scoped.Exec(ctx, insert); e != nil {
		t.Fatal(e)
	}
	if _, e := scoped.Exec(ctx, insert); e == nil {
		t.Fatal("daemon identity uniqueness lost")
	}
	options.Direction = "down"
	options.Files = realMigrationFiles(t, versions[:1], "down")
	options.Hooks = hooksForDirection("down")
	if e := runMigrations(ctx, scoped, options); e == nil || !strings.Contains(e.Error(), "release device maintenance") {
		t.Fatal("active lease fence removal admitted", e)
	}
	if _, e := scoped.Exec(ctx, `DELETE FROM device_maintenance WHERE daemon_id='fixture'`); e != nil {
		t.Fatal(e)
	}
	options.Files = realMigrationFiles(t, []string{versions[2], versions[1], versions[0]}, "down")
	if e := runMigrations(ctx, scoped, options); e != nil {
		t.Fatal(e)
	}
}

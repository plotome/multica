package forkupdate

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestPostgresApprovalSurvivesRestartAndRejectsStaleReports(t *testing.T) {
	dsn := os.Getenv("FORK_UPDATE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("explicit disposable FORK_UPDATE_TEST_DATABASE_URL required")
	}
	u, err := url.Parse(dsn)
	if err != nil || !strings.HasPrefix(u.Path, "/multica_") {
		t.Fatal("disposable test database required")
	}
	pool := testutil.IsolatedDB(t, dsn)
	store := PostgresStore{DB: pool, Transactions: pool}
	ctx := context.Background()
	workspace := uuid.NewString()
	c := verifiedFixture(t)
	receipt, err := Plan(c, inventoryFixture(c))
	if err != nil {
		t.Fatal(err)
	}
	state := ControlState{Receipt: receipt, Revision: 1}
	if err := store.Create(ctx, workspace, state); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Latest(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	stale := loaded
	if err := loaded.Action("approve", receipt.Digest, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, workspace, loaded); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, workspace, stale); !errors.Is(err, ErrConflict) {
		t.Fatal("stale update overwrote approval", err)
	}
	restarted := PostgresStore{DB: pool, Transactions: pool}
	current, err := restarted.Latest(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !current.Receipt.Approved || current.Revision != 2 || current.Receipt.Complete() {
		t.Fatal("approval or partial completion lost")
	}
	if err := store.Create(ctx, workspace, state); !errors.Is(err, ErrConflict) {
		t.Fatal("sequence replay was accepted", err)
	}
}

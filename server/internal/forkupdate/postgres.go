package forkupdate

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrConflict = errors.New("update plan changed; refresh and retry")

type StateDB interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}
type StateTransactions interface {
	Begin(context.Context) (pgx.Tx, error)
}
type PostgresStore struct {
	DB           StateDB
	Transactions StateTransactions
}

func (s PostgresStore) Latest(ctx context.Context, workspace string) (ControlState, error) {
	var state ControlState
	var data []byte
	err := s.DB.QueryRow(ctx, `SELECT body, revision FROM fork_update_rollout WHERE workspace_id=$1 ORDER BY sequence DESC LIMIT 1`, workspace).Scan(&data, &state.Revision)
	if err != nil {
		return state, err
	}
	revision := state.Revision
	if err := decodeStrict(data, &state); err != nil {
		return state, err
	}
	state.Revision = revision
	return state, nil
}

func (s PostgresStore) Create(ctx context.Context, workspace string, state ControlState) error {
	tx, err := s.Transactions.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, workspace); err != nil {
		return err
	}
	var highest int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(sequence),0) FROM fork_update_rollout WHERE workspace_id=$1`, workspace).Scan(&highest); err != nil {
		return err
	}
	if state.Receipt.Sequence <= uint64(highest) {
		return ErrConflict
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO fork_update_rollout(workspace_id,digest,sequence,body,revision) VALUES($1,$2,$3,$4,1)`, workspace, state.Receipt.Digest, state.Receipt.Sequence, data); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s PostgresStore) Save(ctx context.Context, workspace string, state ControlState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	result, err := s.DB.Exec(ctx, `UPDATE fork_update_rollout SET body=$4,revision=revision+1 WHERE workspace_id=$1 AND digest=$2 AND revision=$3`, workspace, state.Receipt.Digest, state.Revision, data)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

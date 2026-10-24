package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"log/slog"
	"time"
)

func (d *Daemon) requestTaskElicitation(ctx context.Context, taskID string, request json.RawMessage, logger *slog.Logger) (json.RawMessage, error) {
	id := uuid.NewString()
	path := fmt.Sprintf("/api/daemon/tasks/%s/elicitations", taskID)
	// A unique ID belongs to this exact request on this exact live connection.
	// Creation is never retried: an ambiguous transport outcome must fail closed.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), taskSupplementAckTimeout)
		defer cancel()
		if err := d.client.postJSON(cleanupCtx, path+"/"+id+"/cancel", map[string]any{}, nil); err != nil {
			logger.Warn("cancel application approval failed", "request_id", id, "error", err)
		}
	}()
	if err := d.client.postJSON(ctx, path, map[string]any{"id": id, "request": request}, nil); err != nil {
		return nil, err
	}
	ticker := time.NewTicker(d.effectiveTaskSupplementPollInterval())
	defer ticker.Stop()
	for {
		var response struct {
			Status   string          `json:"status"`
			Response json.RawMessage `json:"response"`
		}
		if err := d.client.getJSON(ctx, path+"/"+id, &response); err != nil {
			return nil, err
		}
		switch response.Status {
		case "responded":
			return response.Response, nil
		case "cancelled":
			return json.RawMessage(`{"action":"cancel"}`), nil
		case "pending":
		default:
			return nil, fmt.Errorf("unknown application approval status %q", response.Status)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

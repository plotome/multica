package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"log/slog"
)

func (d *Daemon) requestTaskElicitation(ctx context.Context, taskID string, request json.RawMessage, logger *slog.Logger) (json.RawMessage, error) {
	// One bounded round trip. Never wait for a person or retry an ambiguous grant.
	ctx, cancel := context.WithTimeout(ctx, taskSupplementAckTimeout)
	defer cancel()
	var result struct {
		Response json.RawMessage                       `json:"response"`
		Approval *protocol.ApplicationApprovalRequired `json:"approval"`
	}
	path := fmt.Sprintf("/api/daemon/tasks/%s/elicitations", taskID)
	if err := d.client.postJSON(ctx, path, map[string]any{"id": uuid.NewString(), "request": request}, &result); err != nil {
		return nil, err
	}
	if result.Approval != nil {
		return nil, result.Approval
	}
	if len(result.Response) == 0 {
		return nil, fmt.Errorf("application approval server returned no decision")
	}
	return result.Response, nil
}

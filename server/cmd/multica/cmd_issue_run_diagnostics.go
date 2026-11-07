package main

import (
	"context"
	"fmt"
	"net/url"
	"os"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"github.com/spf13/cobra"
)

var issueRunDiagnosticsCmd = &cobra.Command{
	Use:   "run-diagnostics <run-id>",
	Short: "Inspect persisted execution evidence without transcript payloads",
	Long: "Read one run's session pointer, rollout-missing flag and persisted message summary. " +
		"unknown never means the model did not start; empty history is not proof of non-execution. " +
		"This read does not verify transcript completeness or access daemon logs.",
	Args: exactArgs(1),
	RunE: runIssueRunDiagnostics,
}

func runIssueRunDiagnostics(cmd *cobra.Command, args []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	issueID := ""
	if input, _ := cmd.Flags().GetString("issue"); input != "" {
		issue, err := resolveIssueRef(ctx, client, input)
		if err != nil {
			return fmt.Errorf("resolve issue: %w", err)
		}
		issueID = issue.ID
	}
	task, err := resolveTaskRunID(ctx, client, issueID, args[0])
	if err != nil {
		return fmt.Errorf("resolve run: %w", err)
	}
	var wire struct {
		protocol.TaskRunDiagnostics
		SessionRolloutMissing *bool `json:"session_rollout_missing"`
	}
	if err := client.GetJSON(ctx, "/api/tasks/"+url.PathEscape(task.ID)+"/diagnostics", &wire); err != nil {
		return fmt.Errorf("read run diagnostics: %w", err)
	}
	result := wire.TaskRunDiagnostics
	// Missing fields from an older server must not look like a no-activity proof.
	if wire.SessionRolloutMissing == nil || result.Version != 1 || result.TaskID != task.ID || result.Messages == nil ||
		(result.ModelActivity != "unknown" && result.ModelActivity != "observed") ||
		(result.Persistence != "unknown" && result.Persistence != "messages_present" && result.Persistence != "model_activity_persisted") {
		return fmt.Errorf("invalid or unsupported run diagnostics response")
	}
	result.SessionRolloutMissing = *wire.SessionRolloutMissing
	output, _ := cmd.Flags().GetString("output")
	if output == "json" {
		return cli.PrintJSON(os.Stdout, result)
	}
	sessionID := "unknown"
	if result.SessionID != nil {
		sessionID = *result.SessionID
	}
	cli.PrintTable(os.Stdout, []string{"RUN", "STATUS", "SESSION", "ROLLOUT MISSING", "MODEL ACTIVITY", "PERSISTENCE"},
		[][]string{{result.TaskID, result.Status, sessionID, fmt.Sprint(result.SessionRolloutMissing), result.ModelActivity, result.Persistence}})
	return nil
}

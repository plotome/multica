package main

import (
	"context"
	"encoding/json"
	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/spf13/cobra"
	"net/url"
	"os"
)

func init() { rootCmd.AddCommand(newApplicationApprovalCommand()) }

func newApplicationApprovalCommand() *cobra.Command {
	approval := &cobra.Command{Use: "application-approval", Short: "Read or answer deferred application approvals as the human device owner"}
	for _, action := range []string{"get", "respond"} {
		action := action
		cmd := &cobra.Command{Use: action + " <task-id> <request-id>", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newAPIClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := cli.APIContext(context.Background())
			defer cancel()
			path := "/api/tasks/" + url.PathEscape(args[0]) + "/elicitations/" + url.PathEscape(args[1])
			var result any
			if action == "get" {
				err = client.GetJSON(ctx, path, &result)
			} else {
				choice, _ := cmd.Flags().GetString("choice")
				body := map[string]any{"choice": choice}
				receiptPath, _ := cmd.Flags().GetString("card-receipt-file")
				if receiptPath != "" {
					raw, readErr := os.ReadFile(receiptPath)
					if readErr != nil {
						return readErr
					}
					var receipt any
					if err = json.Unmarshal(raw, &receipt); err != nil {
						return err
					}
					body["card_receipt"] = receipt
				}
				err = client.PostJSON(ctx, path+"/respond", body, &result)
			}
			if err != nil {
				return err
			}
			return cli.PrintJSON(os.Stdout, result)
		}}
		if action == "respond" {
			cmd.Flags().String("card-receipt-file", "", "Signed owner receipt from the configured card service")
			cmd.Flags().String("choice", "", "once, session, always, decline or cancel")
			_ = cmd.MarkFlagRequired("choice")
		}
		approval.AddCommand(cmd)
	}
	return approval
}

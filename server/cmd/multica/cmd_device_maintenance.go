package main

import (
	"context"
	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/devicemaintenance"
	"github.com/spf13/cobra"
	"net/url"
)

func init() {
	device := &cobra.Command{Use: "device", Short: "Device-wide maintenance without changing a daemon binary"}
	maintenance := &cobra.Command{Use: "maintenance", Short: "Persistent server-side device fence"}
	for _, action := range []string{"capabilities", "status", "acquire", "drain", "release"} {
		action := action
		cmd := &cobra.Command{Use: action + " <daemon-id>", Args: exactArgs(1), Short: action + " a device maintenance lease"}
		cmd.Flags().String("lease", "", "Persisted lease UUID")
		cmd.Flags().String("digest", "", "Operation policy SHA256")
		cmd.Flags().String("generation", "", "Server-issued generation UUID")
		cmd.Flags().String("output", "json", "Output format (json)")
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			if action != "capabilities" && action != "status" {
				if err := requireHumanLocalCommand("device maintenance " + action); err != nil {
					return err
				}
			}
			client, err := newAPIClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := cli.APIContext(context.Background())
			defer cancel()
			path := "/api/device-maintenance/" + url.PathEscape(args[0]) + "/" + action
			var out devicemaintenance.State
			if action == "capabilities" || action == "status" {
				err = client.GetJSON(ctx, path, &out)
			} else {
				lease, _ := cmd.Flags().GetString("lease")
				digest, _ := cmd.Flags().GetString("digest")
				gen, _ := cmd.Flags().GetString("generation")
				err = client.PostJSON(ctx, path, devicemaintenance.Request{Lease: lease, Digest: digest, Generation: gen}, &out)
			}
			if err != nil {
				return err
			}
			return cli.PrintJSON(cmd.OutOrStdout(), out)
		}
		maintenance.AddCommand(cmd)
	}
	device.AddCommand(maintenance)
	rootCmd.AddCommand(device)
}

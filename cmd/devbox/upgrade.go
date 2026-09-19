package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/nicoalimin/devbox/internal/buildinfo"
	"github.com/nicoalimin/devbox/pkg/client"
	"github.com/spf13/cobra"
)

func upgradeCmd() *cobra.Command {
	var statusOnly, wait bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use: "upgrade", Short: "Build the server's configured main branch, drain jobs, and restart",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newAPIClient()
			var state *client.UpgradeStatus
			var err error
			if statusOnly {
				state, err = c.UpgradeStatus()
			} else {
				state, err = c.Upgrade()
			}
			if err != nil {
				return err
			}
			if !statusOnly && wait {
				ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
				defer cancel()
				state, err = c.WaitForUpgrade(ctx, state.ID, time.Second, func(phase string) {
					if !outputJSON {
						fmt.Fprintln(os.Stderr, "Upgrade:", phase)
					}
				})
				if err != nil {
					return err
				}
			}
			if outputJSON {
				printJSON(state)
				return nil
			}
			fmt.Printf("Upgrade %s: %s\n", state.ID, state.Phase)
			if state.TargetRevision != "" {
				fmt.Printf("Server revision: %s\n", state.TargetRevision)
			}
			if state.Error != "" {
				fmt.Println(state.Error)
			}
			if state.ClientAction != "" {
				fmt.Println(state.ClientAction)
			}
			if state.Phase == "complete" && buildinfo.Revision != state.TargetRevision {
				fmt.Printf("This client was built at %s. Use the updated devbox binary for subsequent commands.\n", buildinfo.Revision)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&statusOnly, "status", false, "show persisted upgrade progress without starting another upgrade")
	cmd.Flags().BoolVar(&wait, "wait", true, "wait through restart and verify server health")
	cmd.Flags().DurationVar(&timeout, "timeout", 2*time.Hour, "client wait limit; timing out does not cancel the server upgrade")
	return cmd
}

func newAPIClient() *client.Client {
	c := client.NewClient(serverURL, token)
	c.SetServerRestartHandler(func(revision, instance string) {
		fmt.Fprintf(os.Stderr, "Server restarted at revision %s (instance %s). Restart long-running clients with the matching devbox binary.\n", revision, instance)
	})
	return c
}

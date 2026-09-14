package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/nicoalimin/devbox/pkg/client"
	"github.com/spf13/cobra"
)

var (
	serverURL   string
	token       string
	outputJSON  bool
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "devbox",
		Short: "Devbox CLI - Manage OpenCode coding agents",
		Long:  "A CLI for interacting with the devboxd server to orchestrate OpenCode coding agents.",
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			// Load config from env if not provided
			if serverURL == "" {
				serverURL = os.Getenv("DEVBOX_SERVER_URL")
			}
			if token == "" {
				token = os.Getenv("DEVBOX_TOKEN")
			}

			// Validate required config
			if cmd.Name() != "help" && cmd.Name() != "version" {
				if serverURL == "" {
					fmt.Fprintln(os.Stderr, "Error: DEVBOX_SERVER_URL is required (via flag or env)")
					os.Exit(1)
				}
				if token == "" {
					fmt.Fprintln(os.Stderr, "Error: DEVBOX_TOKEN is required (via flag or env)")
					os.Exit(1)
				}
			}
		},
	}

	rootCmd.PersistentFlags().StringVar(&serverURL, "server", "", "devboxd server URL (or DEVBOX_SERVER_URL env)")
	rootCmd.PersistentFlags().StringVar(&token, "token", "", "authentication token (or DEVBOX_TOKEN env)")
	rootCmd.PersistentFlags().BoolVar(&outputJSON, "json", false, "output in JSON format")

	rootCmd.AddCommand(
		healthCmd(),
		statusCmd(),
		assignCmd(),
		jobsCmd(),
		jobCmd(),
		blockersCmd(),
		replyCmd(),
		reviewCmd(),
		cancelCmd(),
		logsCmd(),
	)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func healthCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "health",
		Short: "Check if server is reachable",
		Run: func(cmd *cobra.Command, args []string) {
			c := client.NewClient(serverURL, token)
			resp, err := c.Health()
			if err != nil {
				exitError("Health check failed", err)
			}

			if outputJSON {
				printJSON(resp)
			} else {
				fmt.Printf("✓ Server is healthy (version %s)\n", resp.Version)
			}
		},
	}
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Get overall status + current job summary",
		Run: func(cmd *cobra.Command, args []string) {
			c := client.NewClient(serverURL, token)
			resp, err := c.Status()
			if err != nil {
				exitError("Failed to get status", err)
			}

			if outputJSON {
				printJSON(resp)
			} else {
				fmt.Printf("Version: %s\n", resp.Version)
				if resp.Busy {
					fmt.Printf("Status: BUSY\n")
					fmt.Printf("Current Job: %s (state: %s)\n", resp.CurrentJobID, resp.CurrentJobState)
				} else {
					fmt.Printf("Status: IDLE\n")
				}
			}
		},
	}
}

func assignCmd() *cobra.Command {
	var queue bool
	var context string
	var contextFile string
	var notes []string

	cmd := &cobra.Command{
		Use:   "assign <LINEAR_ISSUE_ID>",
		Short: "Create/start job for Linear ticket",
		Long: `Create and start a job for a Linear ticket.

You can provide additional context to the coding agent using:
  --context "inline context"
  --context-file path/to/file
  --note "first note" --note "second note" (can be used multiple times)

All context sources are combined and passed to the OpenCode worker.`,
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			// Build operator context from all sources
			var contextParts []string

			if context != "" {
				contextParts = append(contextParts, context)
			}

			if contextFile != "" {
				fileContent, err := os.ReadFile(contextFile)
				if err != nil {
					exitError("Failed to read context file", err)
				}
				contextParts = append(contextParts, string(fileContent))
			}

			for _, note := range notes {
				contextParts = append(contextParts, note)
			}

			operatorContext := ""
			if len(contextParts) > 0 {
				operatorContext = fmt.Sprintf("%s", contextParts[0])
				for i := 1; i < len(contextParts); i++ {
					operatorContext += "\n\n" + contextParts[i]
				}
			}

			c := client.NewClient(serverURL, token)
			job, err := c.Assign(args[0], operatorContext)
			if err != nil {
				exitError("Failed to assign job", err)
			}

			if outputJSON {
				printJSON(job)
			} else {
				fmt.Printf("✓ Job created: %s\n", job.ID)
				fmt.Printf("  Linear Issue: %s\n", job.LinearIssueID)
				fmt.Printf("  State: %s\n", job.State)
				if operatorContext != "" {
					fmt.Printf("  Operator Context: provided (%d bytes)\n", len(operatorContext))
				}
				fmt.Printf("  Created: %s\n", job.CreatedAt.Format(time.RFC3339))
			}
		},
	}
	cmd.Flags().BoolVar(&queue, "queue", false, "queue if server is busy")
	cmd.Flags().StringVar(&context, "context", "", "additional context for the coding agent")
	cmd.Flags().StringVar(&contextFile, "context-file", "", "path to file containing additional context")
	cmd.Flags().StringArrayVar(&notes, "note", []string{}, "additional note (can be specified multiple times)")
	return cmd
}

func jobsCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "jobs",
		Short: "List recent jobs",
		Run: func(cmd *cobra.Command, args []string) {
			c := client.NewClient(serverURL, token)
			jobs, err := c.ListJobs(limit)
			if err != nil {
				exitError("Failed to list jobs", err)
			}

			if outputJSON {
				printJSON(map[string]interface{}{"jobs": jobs})
			} else {
				if len(jobs) == 0 {
					fmt.Println("No jobs found")
					return
				}

				fmt.Printf("%-36s %-15s %-12s %-20s\n", "JOB ID", "LINEAR ISSUE", "STATE", "CREATED")
				fmt.Println("─────────────────────────────────────────────────────────────────────────────────")
				for _, job := range jobs {
					fmt.Printf("%-36s %-15s %-12s %-20s\n",
						job.ID,
						job.LinearIssueID,
						job.State,
						job.CreatedAt.Format("2006-01-02 15:04:05"))
				}
			}
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 10, "maximum number of jobs to list")
	return cmd
}

func jobCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "job <job_id>",
		Short: "Get detailed job status",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			c := client.NewClient(serverURL, token)
			job, err := c.GetJob(args[0])
			if err != nil {
				exitError("Failed to get job", err)
			}

			if outputJSON {
				printJSON(job)
			} else {
				fmt.Printf("Job ID: %s\n", job.ID)
				fmt.Printf("Linear Issue: %s\n", job.LinearIssueID)
				if job.LinearURL != "" {
					fmt.Printf("Linear URL: %s\n", job.LinearURL)
				}
				fmt.Printf("State: %s\n", job.State)
				if job.RepoPath != "" {
					fmt.Printf("Repository: %s\n", job.RepoPath)
				}
				if job.BranchName != "" {
					fmt.Printf("Branch: %s\n", job.BranchName)
				}
				if job.PRURL != "" {
					fmt.Printf("Pull Request: %s\n", job.PRURL)
				}
				if job.BlockerReason != "" {
					fmt.Printf("Blocker: %s\n", job.BlockerReason)
				}
				fmt.Printf("Created: %s\n", job.CreatedAt.Format(time.RFC3339))
				fmt.Printf("Updated: %s\n", job.UpdatedAt.Format(time.RFC3339))
				if job.CompletedAt != nil {
					fmt.Printf("Completed: %s\n", job.CompletedAt.Format(time.RFC3339))
				}
			}
		},
	}
}

func blockersCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "blockers",
		Short: "List jobs in blocked state",
		Run: func(cmd *cobra.Command, args []string) {
			c := client.NewClient(serverURL, token)
			jobs, err := c.GetBlockers()
			if err != nil {
				exitError("Failed to get blockers", err)
			}

			if outputJSON {
				printJSON(map[string]interface{}{"blockers": jobs})
			} else {
				if len(jobs) == 0 {
					fmt.Println("No blocked jobs")
					return
				}

				fmt.Printf("Blocked Jobs:\n\n")
				for _, job := range jobs {
					fmt.Printf("Job ID: %s\n", job.ID)
					fmt.Printf("  Linear Issue: %s\n", job.LinearIssueID)
					fmt.Printf("  Blocker: %s\n", job.BlockerReason)
					fmt.Printf("  Created: %s\n\n", job.CreatedAt.Format(time.RFC3339))
				}
			}
		},
	}
}

func replyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reply <job_id> <message>",
		Short: "Send clarification to resume blocked job",
		Args:  cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			c := client.NewClient(serverURL, token)
			if err := c.Reply(args[0], args[1]); err != nil {
				exitError("Failed to reply to job", err)
			}

			if outputJSON {
				printJSON(map[string]string{"status": "success"})
			} else {
				fmt.Printf("✓ Reply sent to job %s\n", args[0])
			}
		},
	}
}

func reviewCmd() *cobra.Command {
	var comments string
	var commentsFile string

	cmd := &cobra.Command{
		Use:   "review <job_id|LINEAR_ISSUE_ID>",
		Short: "Send review feedback to update existing PR",
		Long: `Send review feedback to an existing job with a PR.

The feedback will be sent to the OpenCode agent to address on the same branch/PR.
You can provide feedback using:
  --comments "inline feedback"
  --comments-file path/to/file

The job can be identified by job ID or Linear issue ID.`,
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			feedback := comments
			if commentsFile != "" {
				fileContent, err := os.ReadFile(commentsFile)
				if err != nil {
					exitError("Failed to read comments file", err)
				}
				if feedback != "" {
					feedback += "\n\n"
				}
				feedback += string(fileContent)
			}

			if feedback == "" {
				exitError("Review feedback required", fmt.Errorf("use --comments or --comments-file to provide feedback"))
			}

			c := client.NewClient(serverURL, token)
			if err := c.Review(args[0], feedback); err != nil {
				exitError("Failed to send review feedback", err)
			}

			if outputJSON {
				printJSON(map[string]string{"status": "success"})
			} else {
				fmt.Printf("✓ Review feedback sent to job %s\n", args[0])
				fmt.Printf("  OpenCode will address the feedback and update the PR\n")
			}
		},
	}
	cmd.Flags().StringVar(&comments, "comments", "", "review feedback comments")
	cmd.Flags().StringVar(&commentsFile, "comments-file", "", "path to file containing review feedback")
	return cmd
}

func cancelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <job_id>",
		Short: "Cancel a job",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			c := client.NewClient(serverURL, token)
			if err := c.Cancel(args[0]); err != nil {
				exitError("Failed to cancel job", err)
			}

			if outputJSON {
				printJSON(map[string]string{"status": "success"})
			} else {
				fmt.Printf("✓ Job %s cancelled\n", args[0])
			}
		},
	}
}

func logsCmd() *cobra.Command {
	var tail int
	cmd := &cobra.Command{
		Use:   "logs <job_id>",
		Short: "View job logs",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			c := client.NewClient(serverURL, token)
			logs, err := c.GetLogs(args[0], tail)
			if err != nil {
				exitError("Failed to get logs", err)
			}

			if outputJSON {
				printJSON(map[string]interface{}{"logs": logs})
			} else {
				if len(logs) == 0 {
					fmt.Println("No logs found")
					return
				}

				for _, log := range logs {
					fmt.Printf("[%s] [%s] %s\n",
						log.Timestamp.Format("15:04:05"),
						log.Level,
						log.Message)
				}
			}
		},
	}
	cmd.Flags().IntVar(&tail, "tail", 0, "show last N log entries (0 = all)")
	return cmd
}

func printJSON(v interface{}) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func exitError(msg string, err error) {
	fmt.Fprintf(os.Stderr, "Error: %s: %v\n", msg, err)
	os.Exit(1)
}

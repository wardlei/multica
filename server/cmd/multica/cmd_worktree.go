package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var worktreeCmd = &cobra.Command{
	Use:   "worktree",
	Short: "Manage daemon-owned Git worktrees used for agent execution",
}

var worktreeInspectCmd = &cobra.Command{
	Use:   "inspect",
	Short: "Inspect a local Git worktree through the selected daemon",
	RunE:  runWorktreeInspect,
}

var worktreeAddCmd = &cobra.Command{
	Use:   "add",
	Short: "Create a code worktree from a completed inspection",
	RunE:  runWorktreeAdd,
}

var worktreeListCmd = &cobra.Command{
	Use:   "list",
	Short: "List code worktrees in the current workspace",
	RunE:  runWorktreeList,
}

type cliCodeWorktreeInspection struct {
	ID     string  `json:"id"`
	Status string  `json:"status"`
	Error  *string `json:"error"`
}

func init() {
	worktreeInspectCmd.Flags().String("local-path", "", "Absolute path to the existing Git worktree")
	worktreeInspectCmd.Flags().String("daemon-id", "", "Daemon that owns the local path")
	worktreeInspectCmd.Flags().String("output", "json", "Output format: json")
	worktreeAddCmd.Flags().String("inspection", "", "Completed inspection ID")
	worktreeAddCmd.Flags().String("output", "json", "Output format: json")
	worktreeListCmd.Flags().String("output", "table", "Output format: table or json")

	worktreeCmd.AddCommand(worktreeInspectCmd)
	worktreeCmd.AddCommand(worktreeAddCmd)
	worktreeCmd.AddCommand(worktreeListCmd)
}

func worktreeClient(cmd *cobra.Command) (*cli.APIClient, context.Context, context.CancelFunc, error) {
	client, err := newAPIClient(cmd)
	if err != nil {
		return nil, nil, nil, err
	}
	if _, err := requireWorkspaceID(cmd); err != nil {
		return nil, nil, nil, err
	}
	ctx, cancel := cli.APIContext(context.Background())
	return client, ctx, cancel, nil
}

func runWorktreeInspect(cmd *cobra.Command, _ []string) error {
	localPath := strings.TrimSpace(mustString(cmd, "local-path"))
	daemonID := strings.TrimSpace(mustString(cmd, "daemon-id"))
	if localPath == "" || daemonID == "" {
		return fmt.Errorf("--local-path and --daemon-id are required")
	}
	client, ctx, cancel, err := worktreeClient(cmd)
	if err != nil {
		return err
	}
	defer cancel()

	var created cliCodeWorktreeInspection
	if err := client.PostJSON(ctx, "/api/code-worktrees/inspections", map[string]string{
		"daemon_id": daemonID, "local_path": localPath,
	}, &created); err != nil {
		return fmt.Errorf("create inspection: %w", err)
	}
	if created.ID == "" {
		return fmt.Errorf("server returned an empty inspection id")
	}
	if err := invokeLocalWorktreeInspection(cmd, created.ID, localPath); err != nil {
		return err
	}
	var completed cliCodeWorktreeInspection
	if err := client.GetJSON(ctx, "/api/code-worktrees/inspections/"+created.ID, &completed); err != nil {
		return fmt.Errorf("read inspection: %w", err)
	}
	if completed.Status != "completed" {
		if completed.Error != nil && *completed.Error != "" {
			return fmt.Errorf("inspection failed: %s", *completed.Error)
		}
		return fmt.Errorf("inspection finished with status %q", completed.Status)
	}
	return cli.PrintJSON(os.Stdout, completed)
}

func invokeLocalWorktreeInspection(cmd *cobra.Command, inspectionID, localPath string) error {
	body, err := json.Marshal(map[string]string{"inspection_id": inspectionID, "local_path": localPath})
	if err != nil {
		return err
	}
	port := healthPortForProfile(resolveProfile(cmd))
	httpClient := &http.Client{Timeout: 35 * time.Second}
	response, err := httpClient.Post(
		fmt.Sprintf("http://127.0.0.1:%d/code-worktree/inspect", port),
		"application/json", bytes.NewReader(body),
	)
	if err != nil {
		return fmt.Errorf("connect to local daemon: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("local Git inspection failed: %s", strings.TrimSpace(string(message)))
	}
	return nil
}

func runWorktreeAdd(cmd *cobra.Command, _ []string) error {
	inspectionID := strings.TrimSpace(mustString(cmd, "inspection"))
	if inspectionID == "" {
		return fmt.Errorf("--inspection is required")
	}
	client, ctx, cancel, err := worktreeClient(cmd)
	if err != nil {
		return err
	}
	defer cancel()
	var result map[string]any
	if err := client.PostJSON(ctx, "/api/code-worktrees", map[string]string{"inspection_id": inspectionID}, &result); err != nil {
		return fmt.Errorf("create code worktree: %w", err)
	}
	return cli.PrintJSON(os.Stdout, result)
}

func runWorktreeList(cmd *cobra.Command, _ []string) error {
	client, ctx, cancel, err := worktreeClient(cmd)
	if err != nil {
		return err
	}
	defer cancel()
	var response struct {
		Worktrees []struct {
			ID, DaemonID, LocalPath, Branch, HeadSHA string
			IsDirty                                  bool
		} `json:"worktrees"`
	}
	if err := client.GetJSON(ctx, "/api/code-worktrees", &response); err != nil {
		return fmt.Errorf("list code worktrees: %w", err)
	}
	output, _ := cmd.Flags().GetString("output")
	if output == "json" {
		return cli.PrintJSON(os.Stdout, response.Worktrees)
	}
	rows := make([][]string, 0, len(response.Worktrees))
	for _, worktree := range response.Worktrees {
		dirty := "clean"
		if worktree.IsDirty {
			dirty = "dirty"
		}
		rows = append(rows, []string{worktree.ID, worktree.Branch, worktree.HeadSHA, dirty, worktree.LocalPath, worktree.DaemonID})
	}
	cli.PrintTable(os.Stdout, []string{"ID", "BRANCH", "HEAD", "STATE", "PATH", "DAEMON"}, rows)
	return nil
}

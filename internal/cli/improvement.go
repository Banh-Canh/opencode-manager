package cli

import (
	"time"

	"github.com/mickael-menu/opencode-manager/internal/agent"
	"github.com/mickael-menu/opencode-manager/internal/config"
	"github.com/mickael-menu/opencode-manager/internal/workspace"
	"github.com/spf13/cobra"
)

func newImproveCmd(cfg config.Config) *cobra.Command {
	return &cobra.Command{
		Use:   "improve",
		Short: "Open or resume self improvement (use /analyze to start analysis)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			summary, err := workspace.NewRegistry(cfg).EnsureImprovement()
			if err != nil {
				return err
			}
			lifecycle, err := workspace.NewLifecycle(cfg)
			if err != nil {
				return err
			}
			ctx, cancel := cmdContext(15 * time.Minute)
			defer cancel()
			process, err := lifecycle.AttachRuntimeCommand(ctx, summary, agent.OpenCode)
			if err != nil {
				return err
			}
			return runInteractive(process)
		},
	}
}

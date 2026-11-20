// Package judgecmd wires local advisory judge commands.
package judgecmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/app"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmderr"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmdruntime"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
)

// Register attaches the advisory judge command to cr.
func Register(rootCmd *cobra.Command, opts *root.Options) {
	group := &cobra.Command{Use: "judge", Short: "Run advisory code-review judgments"}
	var configPath, resultsDir string
	var jsonOutput bool
	prior := &cobra.Command{
		Use:   "prior-threads <input.json>",
		Short: "Suggest matches to earlier review threads from retained reports",
		Args:  exitcode.ExactArgs(1, "judge prior-threads requires one input path"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(configPath) == "" || strings.TrimSpace(resultsDir) == "" {
				return exitcode.Usage(fmt.Errorf("judge prior-threads requires --config and --results-dir"))
			}
			return run(cmd, opts, args[0], configPath, resultsDir, jsonOutput)
		},
	}
	prior.Flags().StringVar(&configPath, "config", "", "Judge YAML configuration")
	prior.Flags().StringVar(&resultsDir, "results-dir", "", "New private directory for suggestions and receipts")
	root.AddJSONFlag(prior, &jsonOutput)
	group.AddCommand(prior)
	rootCmd.AddCommand(group)
}

func run(cmd *cobra.Command, opts *root.Options, inputPath, configPath, resultsDir string, jsonOutput bool) error {
	// #nosec G304 -- users explicitly supply the local judge fixture.
	file, err := os.Open(inputPath)
	if err != nil {
		return fmt.Errorf("judge: open input: %w", err)
	}
	defer func() { _ = file.Close() }()
	input, err := io.ReadAll(io.LimitReader(file, app.MaxPriorJudgeInputBytes+1))
	if err != nil {
		return fmt.Errorf("judge: read input: %w", err)
	}
	report, err := app.JudgePriorThreads(cmd.Context(), input, configPath, resultsDir,
		func(ctx context.Context, kind string, location config.CredentialLocation) (string, error) {
			return cmdruntime.IdentityKeyResolver(opts, cmderr.BackendFlagChanged(cmd), "judge", kind, location)(ctx)
		})
	if err != nil {
		return err
	}
	if jsonOutput {
		if err := json.NewEncoder(opts.Stdout).Encode(report); err != nil {
			return err
		}
	} else {
		if err := writeReport(opts.Stdout, report, resultsDir); err != nil {
			return err
		}
	}
	if report.Status == "unavailable" {
		return fmt.Errorf("judge: no suggestions available; see receipts in %q", resultsDir)
	}
	return nil
}

func writeReport(w io.Writer, report app.PriorJudgeReport, resultsDir string) error {
	if _, err := fmt.Fprintf(w, "Prior-thread judge %s. %d suggestions. Artifacts: %q\n", report.Status, len(report.Suggestions), resultsDir); err != nil {
		return err
	}
	for _, suggestion := range report.Suggestions {
		if _, err := fmt.Fprintf(w, "Finding %q may match thread %q (%.2f)\n", suggestion.FindingID, suggestion.ThreadID, suggestion.Confidence); err != nil {
			return err
		}
	}
	return nil
}

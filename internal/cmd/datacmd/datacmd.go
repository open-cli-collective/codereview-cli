// Package datacmd wires the `cr data` command surface.
package datacmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/datalifecycle"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/progress"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

type commandFlags struct {
	jsonOutput bool
	dryRun     bool
	olderThan  time.Duration
	keepLast   int
	yes        bool
}

// Register attaches the data command tree to rootCmd.
func Register(rootCmd *cobra.Command, opts *root.Options) {
	cmd := &cobra.Command{
		Use:   "data",
		Short: "Manage local review data",
	}
	cmd.AddCommand(newShowCommand(opts), newPruneCommand(opts), newPurgeCommand(opts))
	rootCmd.AddCommand(cmd)
}

func newShowCommand(opts *root.Options) *cobra.Command {
	var flags commandFlags
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show local review data usage",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return exitcode.Usage(fmt.Errorf("data show accepts no arguments"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			layout, store, cleanup, err := openStore(cmd.Context(), progress.New(nil, true, nil), "data.show", false)
			if err != nil {
				return err
			}
			defer cleanup()
			stats, err := datalifecycle.Show(cmd.Context(), datalifecycle.Options{Layout: layout, Store: store})
			if err != nil {
				return err
			}
			result := view.NewDataShow(stats)
			if flags.jsonOutput {
				return view.RenderDataShowJSON(opts.Stdout, result)
			}
			return view.RenderDataShowText(opts.Stdout, result)
		},
	}
	addJSONFlag(cmd, &flags)
	return cmd
}

func newPruneCommand(opts *root.Options) *cobra.Command {
	var flags commandFlags
	flags.keepLast = -1
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Prune old local review data",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return exitcode.Usage(fmt.Errorf("data prune accepts no arguments"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger := newProgressLogger(opts)
			olderThanChanged := cmd.Flags().Changed("older-than")
			keepLastChanged := cmd.Flags().Changed("keep-last")
			if flags.olderThan > 0 && keepLastChanged {
				return exitcode.Usage(fmt.Errorf("--older-than and --keep-last are mutually exclusive"))
			}
			if olderThanChanged && flags.olderThan <= 0 {
				return exitcode.Usage(fmt.Errorf("--older-than must be positive"))
			}
			if keepLastChanged && flags.keepLast < 0 {
				return exitcode.Usage(fmt.Errorf("--keep-last must be non-negative"))
			}
			var keepLast *int
			if keepLastChanged {
				keepLast = &flags.keepLast
			}

			layout, store, cleanup, err := openStore(cmd.Context(), logger, "data.prune", !flags.dryRun)
			if err != nil {
				return err
			}
			defer cleanup()
			result, err := datalifecycle.Prune(cmd.Context(), datalifecycle.Options{
				Layout:   layout,
				Store:    store,
				Progress: lifecycleProgressReporter{logger: logger, command: "data.prune"},
			}, datalifecycle.PruneOptions{
				OlderThan: flags.olderThan,
				KeepLast:  keepLast,
				DryRun:    flags.dryRun,
			})
			if err != nil {
				return err
			}
			if flags.dryRun {
				legacySpan := logger.Start("data.prune", "check_legacy", "data-root")
				legacyExists, err := statepaths.LegacyDataRootExists(layout)
				if err != nil {
					return endProgressSpan(legacySpan, err)
				}
				if legacyExists {
					legacyRoot := statepaths.LegacyDataRoot(layout)
					result.Warnings = append(result.Warnings, fmt.Sprintf("legacy data exists at %s or %s.migrating; dry-run does not migrate it, so this preview excludes legacy runs until a write command migrates them", legacyRoot, legacyRoot))
				}
				legacySpan.End(nil)
			}
			rendered := view.NewDataPrune(result)
			if flags.jsonOutput {
				return view.RenderDataPruneJSON(opts.Stdout, rendered)
			}
			return view.RenderDataPruneText(opts.Stdout, rendered)
		},
	}
	addJSONFlag(cmd, &flags)
	cmd.Flags().BoolVar(&flags.dryRun, "dry-run", false, "Report data that would be pruned without deleting")
	cmd.Flags().DurationVar(&flags.olderThan, "older-than", 0, "Prune runs older than this duration")
	cmd.Flags().IntVar(&flags.keepLast, "keep-last", -1, "Keep the newest N runs per post mode")
	return cmd
}

func newPurgeCommand(opts *root.Options) *cobra.Command {
	var flags commandFlags
	cmd := &cobra.Command{
		Use:   "purge",
		Short: "Purge all local review data",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return exitcode.Usage(fmt.Errorf("data purge accepts no arguments"))
			}
			return nil
		},
		RunE: func(_ *cobra.Command, _ []string) error {
			logger := newProgressLogger(opts)
			if flags.yes && flags.dryRun {
				return exitcode.Usage(fmt.Errorf("--yes and --dry-run are mutually exclusive"))
			}
			layoutSpan := logger.Start("data.purge", "resolve_layout", "data-root")
			layout, err := statepaths.DefaultLayout()
			if err != nil {
				return endProgressSpan(layoutSpan, err)
			}
			layoutSpan.End(nil)
			purgeSpan := logger.Start("data.purge", "purge_root", "data-root")
			result, err := datalifecycle.Purge(layout, flags.dryRun, flags.yes, nil)
			if err != nil {
				return exitcode.Usage(endProgressSpan(purgeSpan, err))
			}
			purgeSpan.End(nil)
			rendered := view.NewDataPurge(result)
			if flags.jsonOutput {
				return view.RenderDataPurgeJSON(opts.Stdout, rendered)
			}
			return view.RenderDataPurgeText(opts.Stdout, rendered)
		},
	}
	addJSONFlag(cmd, &flags)
	cmd.Flags().BoolVar(&flags.dryRun, "dry-run", false, "Report the data root without deleting")
	cmd.Flags().BoolVar(&flags.yes, "yes", false, "Confirm permanent deletion")
	return cmd
}

func addJSONFlag(cmd *cobra.Command, flags *commandFlags) {
	cmd.Flags().BoolVar(&flags.jsonOutput, "json", false, "Emit JSON")
}

func openStore(ctx context.Context, logger *progress.Logger, command string, migrateLegacyData bool) (statepaths.Layout, datalifecycle.Store, func(), error) {
	layoutSpan := logger.Start(command, "resolve_layout", "data-root")
	layout, err := statepaths.DefaultLayout()
	if err != nil {
		return statepaths.Layout{}, nil, nil, endProgressSpan(layoutSpan, err)
	}
	layoutSpan.End(nil)
	if migrateLegacyData {
		migrateSpan := logger.Start(command, "migrate_legacy", "data-root")
		if err := statepaths.MigrateLegacyDataRoot(layout); err != nil {
			return statepaths.Layout{}, nil, nil, endProgressSpan(migrateSpan, err)
		}
		migrateSpan.End(nil)
	}
	ledgerSpan := logger.Start(command, "open_ledger", "data-root")
	if _, err := os.Stat(layout.LedgerDB()); errors.Is(err, os.ErrNotExist) {
		ledgerSpan.End(nil)
		return layout, emptyLifecycleStore{}, func() {}, nil
	} else if err != nil {
		return statepaths.Layout{}, nil, nil, endProgressSpan(ledgerSpan, err)
	}
	store, err := ledger.Open(ctx, layout.LedgerDB())
	if err != nil {
		return statepaths.Layout{}, nil, nil, endProgressSpan(ledgerSpan, err)
	}
	ledgerSpan.End(nil)
	return layout, store, func() { _ = store.Close() }, nil
}

type lifecycleProgressReporter struct {
	logger  *progress.Logger
	command string
}

func (r lifecycleProgressReporter) Start(op, target string) datalifecycle.ProgressSpan {
	return r.logger.Start(r.command, op, target)
}

func newProgressLogger(opts *root.Options) *progress.Logger {
	if opts == nil {
		return progress.New(nil, true, nil)
	}
	return progress.New(opts.Stderr, opts.Quiet, nil)
}

func endProgressSpan(span *progress.Span, err error) error {
	if span != nil {
		span.End(err)
	}
	return err
}

type emptyLifecycleStore struct{}

func (emptyLifecycleStore) ListRuns(context.Context) ([]ledger.Run, error) {
	return nil, nil
}

func (emptyLifecycleStore) DeleteRun(context.Context, string) error {
	return ledger.ErrNotFound
}

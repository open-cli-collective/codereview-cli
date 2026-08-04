// Package datacmd wires the `cr data` command surface.
package datacmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/datalifecycle"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/progress"
	"github.com/open-cli-collective/codereview-cli/internal/runlock"
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
		Args:  exitcode.NoArgs("data show accepts no arguments"),
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
			return view.Render(opts.Stdout, flags.jsonOutput, result, func(w io.Writer) error {
				return view.RenderDataShowText(w, result)
			})
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
		Args:  exitcode.NoArgs("data prune accepts no arguments"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger := root.NewProgressLogger(opts)
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
			if !flags.dryRun {
				lock, lockErr := acquireDataLock(layout, "data prune")
				if lockErr != nil {
					return lockErr
				}
				defer func() { _ = lock.Release() }()
			}
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
					return legacySpan.End(err)
				}
				if legacyExists {
					legacyRoot := statepaths.LegacyDataRoot(layout)
					result.Warnings = append(result.Warnings, fmt.Sprintf("legacy data exists at %s or %s.migrating; dry-run does not migrate it, so this preview excludes legacy runs until a write command migrates them", legacyRoot, legacyRoot))
				}
				_ = legacySpan.End(nil)
			}
			rendered := view.NewDataPrune(result)
			return view.Render(opts.Stdout, flags.jsonOutput, rendered, func(w io.Writer) error {
				return view.RenderDataPruneText(w, rendered)
			})
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
		Args:  exitcode.NoArgs("data purge accepts no arguments"),
		RunE: func(_ *cobra.Command, _ []string) error {
			logger := root.NewProgressLogger(opts)
			if flags.yes && flags.dryRun {
				return exitcode.Usage(fmt.Errorf("--yes and --dry-run are mutually exclusive"))
			}
			layoutSpan := logger.Start("data.purge", "resolve_layout", "data-root")
			layout, err := statepaths.DefaultLayout()
			if err != nil {
				return layoutSpan.End(err)
			}
			_ = layoutSpan.End(nil)
			if !flags.dryRun {
				lock, lockErr := acquireDataLock(layout, "data purge")
				if lockErr != nil {
					return lockErr
				}
				defer func() { _ = lock.Release() }()
			}
			purgeSpan := logger.Start("data.purge", "purge_root", "data-root")
			result, err := datalifecycle.Purge(layout, flags.dryRun, flags.yes, nil)
			if err != nil {
				return exitcode.Usage(purgeSpan.End(err))
			}
			_ = purgeSpan.End(nil)
			rendered := view.NewDataPurge(result)
			return view.Render(opts.Stdout, flags.jsonOutput, rendered, func(w io.Writer) error {
				return view.RenderDataPurgeText(w, rendered)
			})
		},
	}
	addJSONFlag(cmd, &flags)
	cmd.Flags().BoolVar(&flags.dryRun, "dry-run", false, "Report the data root without deleting")
	cmd.Flags().BoolVar(&flags.yes, "yes", false, "Confirm permanent deletion")
	return cmd
}

func addJSONFlag(cmd *cobra.Command, flags *commandFlags) {
	root.AddJSONFlag(cmd, &flags.jsonOutput)
}

func openStore(ctx context.Context, logger *progress.Logger, command string, migrateLegacyData bool) (statepaths.Layout, datalifecycle.Store, func(), error) {
	layout, store, cleanup, err := ledger.OpenLedger(ctx, logger, command, "data-root", migrateLegacyData)
	if err != nil {
		return statepaths.Layout{}, nil, nil, err
	}
	if store == nil {
		return layout, emptyLifecycleStore{}, cleanup, nil
	}
	return layout, store, cleanup, nil
}

type lifecycleProgressReporter struct {
	logger  *progress.Logger
	command string
}

func (r lifecycleProgressReporter) Start(op, target string) datalifecycle.ProgressSpan {
	return lifecycleProgressSpan{r.logger.Start(r.command, op, target)}
}

type lifecycleProgressSpan struct{ *progress.Span }

func (s lifecycleProgressSpan) End(err error) { _ = s.Span.End(err) }

type emptyLifecycleStore struct{}

func (emptyLifecycleStore) ListRuns(context.Context) ([]ledger.Run, error) {
	return nil, nil
}

func (emptyLifecycleStore) DeleteRun(context.Context, string) error {
	return ledger.ErrNotFound
}

// acquireDataLock takes the data-root active-runs lock before a destructive
// lifecycle command so it cannot delete a live run's artifacts. A missing
// data root returns no lock: there is nothing to protect, and creating the
// lock file would materialize state the command promises not to create.
func acquireDataLock(layout statepaths.Layout, op string) (*runlock.Lock, error) {
	if _, err := os.Stat(layout.DataRoot); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	lock, err := runlock.Acquire(layout.ActiveRunsLock())
	if err != nil {
		if errors.Is(err, runlock.ErrHeld) {
			return nil, fmt.Errorf("%s: another cr instance appears to be running; wait for it to finish and retry", op)
		}
		return nil, err
	}
	return lock, nil
}

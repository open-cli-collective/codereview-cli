// Package sessionscmd wires the `cr sessions` command surface.
package sessionscmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/progress"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

type commandFlags struct {
	jsonOutput bool
}

// Register attaches the sessions command tree to rootCmd.
func Register(rootCmd *cobra.Command, opts *root.Options) {
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "Manage named LLM sessions",
	}
	cmd.AddCommand(newListCommand(opts), newShowCommand(opts), newDeleteCommand(opts))
	rootCmd.AddCommand(cmd)
}

func newListCommand(opts *root.Options) *cobra.Command {
	var flags commandFlags
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List named LLM sessions",
		Args:  exitcode.NoArgs("sessions list accepts no arguments"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, cleanup, err := openStore(cmd.Context(), nil, "sessions.list", false)
			if err != nil {
				return err
			}
			defer cleanup()
			if store == nil {
				result := view.NewSessionsList(nil)
				if flags.jsonOutput {
					return view.RenderSessionsListJSON(opts.Stdout, result)
				}
				return view.RenderSessionsListText(opts.Stdout, result)
			}
			sessions, err := store.ListNamedSessions(cmd.Context())
			if err != nil {
				return err
			}
			result := view.NewSessionsList(sessions)
			if flags.jsonOutput {
				return view.RenderSessionsListJSON(opts.Stdout, result)
			}
			return view.RenderSessionsListText(opts.Stdout, result)
		},
	}
	addCommonFlags(cmd, &flags)
	return cmd
}

func newShowCommand(opts *root.Options) *cobra.Command {
	var flags commandFlags
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show one named LLM session",
		Args:  exitcode.NonEmptyArg("sessions show requires <name>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			store, cleanup, err := openStore(cmd.Context(), nil, "sessions.show", false)
			if err != nil {
				return err
			}
			defer cleanup()
			if store == nil {
				return exitcode.With(exitcode.Failure, fmt.Errorf("session %q not found", name))
			}
			session, err := store.GetNamedSession(cmd.Context(), name)
			if errors.Is(err, ledger.ErrNotFound) {
				return exitcode.With(exitcode.Failure, fmt.Errorf("session %q not found", name))
			}
			if err != nil {
				return err
			}
			result := view.NewSessionsShow(session)
			if flags.jsonOutput {
				return view.RenderSessionsShowJSON(opts.Stdout, result)
			}
			return view.RenderSessionsShowText(opts.Stdout, result)
		},
	}
	addCommonFlags(cmd, &flags)
	return cmd
}

func newDeleteCommand(opts *root.Options) *cobra.Command {
	var flags commandFlags
	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete one named LLM session",
		Args:  exitcode.NonEmptyArg("sessions delete requires <name>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			logger := newProgressLogger(opts)
			store, cleanup, err := openStore(cmd.Context(), logger, "sessions.delete", true)
			if err != nil {
				return err
			}
			defer cleanup()
			if store == nil {
				return exitcode.With(exitcode.Failure, fmt.Errorf("session %q not found", name))
			}
			deleteSpan := logger.Start("sessions.delete", "delete_session", "session")
			if err := store.DeleteNamedSession(cmd.Context(), name); errors.Is(err, ledger.ErrNotFound) {
				deleteSpan.End(err)
				return exitcode.With(exitcode.Failure, fmt.Errorf("session %q not found", name))
			} else if err != nil {
				deleteSpan.End(err)
				return err
			}
			deleteSpan.End(nil)
			result := view.NewSessionsDelete(name)
			if flags.jsonOutput {
				return view.RenderSessionsDeleteJSON(opts.Stdout, result)
			}
			return view.RenderSessionsDeleteText(opts.Stdout, result)
		},
	}
	addCommonFlags(cmd, &flags)
	return cmd
}

func addCommonFlags(cmd *cobra.Command, flags *commandFlags) {
	cmd.Flags().BoolVar(&flags.jsonOutput, "json", false, "Emit JSON")
}

func openStore(ctx context.Context, logger *progress.Logger, command string, migrateLegacyData bool) (*ledger.Store, func(), error) {
	if logger == nil {
		logger = progress.New(nil, true, nil)
	}
	layoutSpan := logger.Start(command, "resolve_layout", "session")
	layout, err := statepaths.DefaultLayout()
	if err != nil {
		return nil, nil, endProgressSpan(layoutSpan, err)
	}
	layoutSpan.End(nil)
	if migrateLegacyData {
		migrateSpan := logger.Start(command, "migrate_legacy", "session")
		if err := statepaths.MigrateLegacyDataRoot(layout); err != nil {
			return nil, nil, endProgressSpan(migrateSpan, err)
		}
		migrateSpan.End(nil)
	}
	ledgerSpan := logger.Start(command, "open_ledger", "session")
	if _, err := os.Stat(layout.LedgerDB()); errors.Is(err, os.ErrNotExist) {
		ledgerSpan.End(nil)
		return nil, func() {}, nil
	} else if err != nil {
		return nil, nil, endProgressSpan(ledgerSpan, err)
	}
	store, err := ledger.Open(ctx, layout.LedgerDB())
	if err != nil {
		return nil, nil, endProgressSpan(ledgerSpan, err)
	}
	ledgerSpan.End(nil)
	return store, func() { _ = store.Close() }, nil
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

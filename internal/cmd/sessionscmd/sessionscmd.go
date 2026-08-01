// Package sessionscmd wires the `cr sessions` command surface.
package sessionscmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/progress"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

type commandFlags struct {
	jsonOutput bool
}

// Register attaches the sessions command tree to rootCmd.
func Register(rootCmd *cobra.Command, opts *root.Options) {
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "Manage named orchestrator sessions",
		Long:  "Manage named orchestrator sessions. PR-scoped reviewer cohorts are reused automatically and reset with cr review --fresh-session.",
	}
	cmd.AddCommand(newListCommand(opts), newShowCommand(opts), newDeleteCommand(opts))
	rootCmd.AddCommand(cmd)
}

func newListCommand(opts *root.Options) *cobra.Command {
	var flags commandFlags
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List named orchestrator sessions",
		Args:  exitcode.NoArgs("sessions list accepts no arguments"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, cleanup, err := openStore(cmd.Context(), nil, "sessions.list", false)
			if err != nil {
				return err
			}
			defer cleanup()
			if store == nil {
				result := view.NewSessionsList(nil)
				return view.Render(opts.Stdout, flags.jsonOutput, result, func(w io.Writer) error {
					return view.RenderSessionsListText(w, result)
				})
			}
			sessions, err := store.ListNamedSessions(cmd.Context())
			if err != nil {
				return err
			}
			result := view.NewSessionsList(sessions)
			return view.Render(opts.Stdout, flags.jsonOutput, result, func(w io.Writer) error {
				return view.RenderSessionsListText(w, result)
			})
		},
	}
	addCommonFlags(cmd, &flags)
	return cmd
}

func newShowCommand(opts *root.Options) *cobra.Command {
	var flags commandFlags
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show one named orchestrator session",
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
			return view.Render(opts.Stdout, flags.jsonOutput, result, func(w io.Writer) error {
				return view.RenderSessionsShowText(w, result)
			})
		},
	}
	addCommonFlags(cmd, &flags)
	return cmd
}

func newDeleteCommand(opts *root.Options) *cobra.Command {
	var flags commandFlags
	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete one named orchestrator session",
		Args:  exitcode.NonEmptyArg("sessions delete requires <name>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			logger := root.NewProgressLogger(opts)
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
				_ = deleteSpan.End(err)
				return exitcode.With(exitcode.Failure, fmt.Errorf("session %q not found", name))
			} else if err != nil {
				_ = deleteSpan.End(err)
				return err
			}
			_ = deleteSpan.End(nil)
			result := view.NewSessionsDelete(name)
			return view.Render(opts.Stdout, flags.jsonOutput, result, func(w io.Writer) error {
				return view.RenderSessionsDeleteText(w, result)
			})
		},
	}
	addCommonFlags(cmd, &flags)
	return cmd
}

func addCommonFlags(cmd *cobra.Command, flags *commandFlags) {
	root.AddJSONFlag(cmd, &flags.jsonOutput)
}

func openStore(ctx context.Context, logger *progress.Logger, command string, migrateLegacyData bool) (*ledger.Store, func(), error) {
	_, store, cleanup, err := ledger.OpenLedger(ctx, logger, command, "session", migrateLegacyData)
	return store, cleanup, err
}

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
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return exitcode.Usage(fmt.Errorf("sessions list accepts no arguments"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, cleanup, err := openStore(cmd.Context(), false, false)
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
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
				return exitcode.Usage(fmt.Errorf("sessions show requires <name>"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			store, cleanup, err := openStore(cmd.Context(), false, false)
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
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
				return exitcode.Usage(fmt.Errorf("sessions delete requires <name>"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			store, cleanup, err := openStore(cmd.Context(), true, false)
			if err != nil {
				return err
			}
			defer cleanup()
			if store == nil {
				return exitcode.With(exitcode.Failure, fmt.Errorf("session %q not found", name))
			}
			if err := store.DeleteNamedSession(cmd.Context(), name); errors.Is(err, ledger.ErrNotFound) {
				return exitcode.With(exitcode.Failure, fmt.Errorf("session %q not found", name))
			} else if err != nil {
				return err
			}
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

func openStore(ctx context.Context, migrateLegacyData bool, create bool) (*ledger.Store, func(), error) {
	layout, err := statepaths.DefaultLayoutEnsured()
	if err != nil {
		return nil, nil, err
	}
	if migrateLegacyData {
		if err := statepaths.MigrateLegacyDataRoot(layout); err != nil {
			return nil, nil, err
		}
	}
	if !create {
		if _, err := os.Stat(layout.LedgerDB()); errors.Is(err, os.ErrNotExist) {
			return nil, func() {}, nil
		} else if err != nil {
			return nil, nil, err
		}
	}
	store, err := ledger.Open(ctx, layout.LedgerDB())
	if err != nil {
		return nil, nil, err
	}
	return store, func() { _ = store.Close() }, nil
}

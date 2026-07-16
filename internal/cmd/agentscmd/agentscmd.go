// Package agentscmd wires the `cr agents` command surface.
package agentscmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/agents"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmderr"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmdruntime"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/gitproviders"
	"github.com/open-cli-collective/codereview-cli/internal/prref"
	"github.com/open-cli-collective/codereview-cli/internal/reporoot"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

// ProviderFactory builds the git provider used by PR-aware agent loading.
type ProviderFactory func(cmd *cobra.Command, opts *root.Options, cfg config.File, profile config.Profile) (gitprovider.GitProvider, func(), error)

type commandFlags struct {
	agentsDirs []string
	jsonOutput bool
}

// Register attaches the agents command tree to rootCmd.
func Register(rootCmd *cobra.Command, opts *root.Options) {
	RegisterWithFactory(rootCmd, opts, newConfiguredProvider)
}

// RegisterWithFactory attaches the agents command tree with an injected provider factory.
func RegisterWithFactory(rootCmd *cobra.Command, opts *root.Options, factory ProviderFactory) {
	cmd := &cobra.Command{
		Use:   "agents",
		Short: "Inspect trusted review agents",
	}
	cmd.AddCommand(newListCommand(opts, factory), newShowCommand(opts, factory))
	rootCmd.AddCommand(cmd)
}

func newListCommand(opts *root.Options, factory ProviderFactory) *cobra.Command {
	var flags commandFlags
	cmd := &cobra.Command{
		Use:   "list [PR]",
		Short: "List trusted review agents",
		Args:  exitcode.MaximumArgs(1, "agents list accepts at most one PR argument"),
		RunE: func(cmd *cobra.Command, args []string) error {
			prArg := ""
			if len(args) == 1 {
				prArg = args[0]
			}
			catalog, err := buildCatalog(cmd.Context(), cmd, opts, factory, flags, prArg)
			if err != nil {
				return err
			}
			result := view.NewAgentsList(catalog)
			return view.Render(opts.Stdout, flags.jsonOutput, result, func(w io.Writer) error {
				return view.RenderAgentsListText(w, result)
			})
		},
	}
	addCommonFlags(cmd, &flags)
	return cmd
}

func newShowCommand(opts *root.Options, factory ProviderFactory) *cobra.Command {
	var flags commandFlags
	cmd := &cobra.Command{
		Use:   "show <name> [PR]",
		Short: "Show one trusted review agent",
		Args:  exitcode.RangeArgs(1, 2, "agents show requires <name> and optional PR"),
		RunE: func(cmd *cobra.Command, args []string) error {
			prArg := ""
			if len(args) == 2 {
				prArg = args[1]
			}
			catalog, err := buildCatalog(cmd.Context(), cmd, opts, factory, flags, prArg)
			if err != nil {
				return err
			}
			agent, ok := catalog.Find(args[0])
			if !ok {
				return exitcode.With(exitcode.Failure, fmt.Errorf("%w: %s", agents.ErrNotFound, args[0]))
			}
			result := view.NewAgentsShow(agent, catalog)
			return view.Render(opts.Stdout, flags.jsonOutput, result, func(w io.Writer) error {
				return view.RenderAgentsShowText(w, result)
			})
		},
	}
	addCommonFlags(cmd, &flags)
	return cmd
}

func addCommonFlags(cmd *cobra.Command, flags *commandFlags) {
	cmd.Flags().StringArrayVar(&flags.agentsDirs, "agents-dir", nil, "Additional trusted agents directory")
	root.AddJSONFlag(cmd, &flags.jsonOutput)
}

// buildCatalog loads config, resolves the active profile, optionally resolves a PR,
// manages provider lifetime, and loads the trusted agent catalog.
func buildCatalog(ctx context.Context, cmd *cobra.Command, opts *root.Options, factory ProviderFactory, flags commandFlags, prArg string) (agents.Catalog, error) {
	path, err := cmdruntime.ConfigPath(opts)
	if err != nil {
		return agents.Catalog{}, exitcode.AuthConfig(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		return agents.Catalog{}, cmderr.Config(err)
	}
	var ref gitprovider.PRRef
	var urlProvider prref.Provider
	profile := config.Profile{}
	hasPR := strings.TrimSpace(prArg) != ""
	if hasPR {
		var err error
		ref, urlProvider, err = prref.ParsePullURL(prArg)
		if err != nil {
			return agents.Catalog{}, exitcode.Usage(err)
		}
		_, profile, err = config.ResolveProfileForRepository(cfg, opts.Profile, root.ProfileFlagChanged(cmd), config.RepositoryTarget{
			Host:      ref.Host,
			Namespace: ref.Owner,
			Repo:      ref.Repo,
		})
		if err != nil {
			return agents.Catalog{}, cmderr.Config(err)
		}
	} else {
		var err error
		_, profile, err = config.ResolveProfile(cfg, opts.Profile)
		if err != nil {
			return agents.Catalog{}, cmderr.Config(err)
		}
	}
	loadOptions := agents.LoadOptions{
		ProfileDirs: append([]string(nil), profile.AgentSources...),
		FlagDirs:    append([]string(nil), flags.agentsDirs...),
	}
	if hasPR {
		invocationRoot, err := reporoot.Resolve(ctx, "", nil)
		if errors.Is(err, reporoot.ErrUnavailable) {
			invocationRoot = ""
		} else if err != nil {
			return agents.Catalog{}, exitcode.Usage(err)
		}
		loadOptions.RequireSafeProfileSources = true
		loadOptions.SafeProfileSourceRoot = invocationRoot
		if !prref.SameHost(ref.Host, profile.Git.Host) {
			return agents.Catalog{}, exitcode.Usage(fmt.Errorf("PR host %q must match configured git host %q", ref.Host, profile.Git.Host))
		}
		if err := prref.MatchProvider(urlProvider, string(profile.Git.ProviderKind())); err != nil {
			return agents.Catalog{}, exitcode.Usage(err)
		}
		provider, cleanup, err := factory(cmd, opts, cfg, profile)
		if err != nil {
			return agents.Catalog{}, err
		}
		if cleanup != nil {
			defer cleanup()
		}
		pr, err := provider.GetPR(ctx, ref)
		if err != nil {
			return agents.Catalog{}, cmderr.Provider(err)
		}
		loadOptions.Repo = &agents.RepoSource{Reader: provider, Ref: ref, PR: pr}
	}

	catalog, err := agents.Load(ctx, loadOptions)
	if err != nil {
		return agents.Catalog{}, mapLoadError(err)
	}
	return catalog, nil
}

func mapLoadError(err error) error {
	switch {
	case errors.Is(err, gitprovider.ErrAuth),
		errors.Is(err, gitprovider.ErrPermission),
		errors.Is(err, gitprovider.ErrRetryable),
		errors.Is(err, gitprovider.ErrNotFound),
		errors.Is(err, gitprovider.ErrConflict),
		errors.Is(err, gitprovider.ErrStaleSHA):
		return cmderr.Provider(err)
	case errors.Is(err, agents.ErrInvalid), errors.Is(err, agents.ErrUnsafeSource):
		return exitcode.Usage(err)
	default:
		return err
	}
}

func newConfiguredProvider(cmd *cobra.Command, opts *root.Options, cfg config.File, profile config.Profile) (gitprovider.GitProvider, func(), error) {
	store, err := credentials.OpenStore(opts.Backend, cmderr.BackendFlagChanged(cmd), cfg)
	if err != nil {
		return nil, nil, cmderr.Credential(err)
	}
	client, _, err := gitproviders.New(profile.Git, store, gitproviders.Options{})
	if err != nil {
		_ = store.Close()
		if errors.Is(err, config.ErrUnsupported) {
			return nil, nil, cmderr.Config(err)
		}
		return nil, nil, mapLoadError(err)
	}
	return client, func() { _ = store.Close() }, nil
}

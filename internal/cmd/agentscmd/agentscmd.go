// Package agentscmd wires the `cr agents` command surface.
package agentscmd

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/agents"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmderr"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	githubprovider "github.com/open-cli-collective/codereview-cli/internal/gitprovider/github"
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
	RegisterWithFactory(rootCmd, opts, newGitHubProvider)
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
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 1 {
				return exitcode.Usage(fmt.Errorf("agents list accepts at most one PR argument"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			prArg := ""
			if len(args) == 1 {
				prArg = args[0]
			}
			catalog, err := loadCatalog(cmd.Context(), cmd, opts, factory, flags, prArg)
			if err != nil {
				return err
			}
			result := view.NewAgentsList(catalog)
			if flags.jsonOutput {
				return view.RenderAgentsListJSON(opts.Stdout, result)
			}
			return view.RenderAgentsListText(opts.Stdout, result)
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
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) < 1 || len(args) > 2 {
				return exitcode.Usage(fmt.Errorf("agents show requires <name> and optional PR"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			prArg := ""
			if len(args) == 2 {
				prArg = args[1]
			}
			catalog, err := loadCatalog(cmd.Context(), cmd, opts, factory, flags, prArg)
			if err != nil {
				return err
			}
			agent, ok := catalog.Find(args[0])
			if !ok {
				return exitcode.With(exitcode.Failure, fmt.Errorf("%w: %s", agents.ErrNotFound, args[0]))
			}
			result := view.NewAgentsShow(agent, catalog)
			if flags.jsonOutput {
				return view.RenderAgentsShowJSON(opts.Stdout, result)
			}
			return view.RenderAgentsShowText(opts.Stdout, result)
		},
	}
	addCommonFlags(cmd, &flags)
	return cmd
}

func addCommonFlags(cmd *cobra.Command, flags *commandFlags) {
	cmd.Flags().StringArrayVar(&flags.agentsDirs, "agents-dir", nil, "Additional trusted agents directory")
	cmd.Flags().BoolVar(&flags.jsonOutput, "json", false, "Emit JSON")
}

func loadCatalog(ctx context.Context, cmd *cobra.Command, opts *root.Options, factory ProviderFactory, flags commandFlags, prArg string) (agents.Catalog, error) {
	path, err := configPath(opts)
	if err != nil {
		return agents.Catalog{}, exitcode.AuthConfig(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		return agents.Catalog{}, cmderr.Config(err)
	}
	_, profile, err := config.ResolveProfile(cfg, opts.Profile)
	if err != nil {
		return agents.Catalog{}, cmderr.Config(err)
	}

	loadOptions := agents.LoadOptions{
		ProfileDirs: append([]string(nil), profile.AgentSources...),
		FlagDirs:    append([]string(nil), flags.agentsDirs...),
	}
	if strings.TrimSpace(prArg) != "" {
		ref, err := parsePRRef(prArg)
		if err != nil {
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
	case errors.Is(err, agents.ErrInvalid):
		return exitcode.Usage(err)
	default:
		return err
	}
}

func parsePRRef(raw string) (gitprovider.PRRef, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return gitprovider.PRRef{}, fmt.Errorf("PR must be a GitHub pull request URL")
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "pull" {
		return gitprovider.PRRef{}, fmt.Errorf("PR must be a GitHub pull request URL")
	}
	number, err := strconv.Atoi(parts[3])
	if err != nil || number <= 0 {
		return gitprovider.PRRef{}, fmt.Errorf("PR number must be positive")
	}
	ref := gitprovider.PRRef{
		Host:   parsed.Host,
		Owner:  parts[0],
		Repo:   parts[1],
		Number: number,
	}
	if err := ref.Validate(); err != nil {
		return gitprovider.PRRef{}, err
	}
	return ref, nil
}

func configPath(opts *root.Options) (string, error) {
	if opts != nil && opts.ConfigPath != "" {
		return opts.ConfigPath, nil
	}
	return config.Path()
}

func newGitHubProvider(cmd *cobra.Command, opts *root.Options, cfg config.File, profile config.Profile) (gitprovider.GitProvider, func(), error) {
	store, err := credentials.OpenStore(opts.Backend, backendFlagChanged(cmd), cfg)
	if err != nil {
		return nil, nil, cmderr.Credential(err)
	}
	client, _, err := githubprovider.NewFromGitConfig(profile.Git, store, githubprovider.Options{})
	if err != nil {
		_ = store.Close()
		if errors.Is(err, config.ErrUnsupported) {
			return nil, nil, cmderr.Config(err)
		}
		return nil, nil, mapLoadError(err)
	}
	return client, func() { _ = store.Close() }, nil
}

func backendFlagChanged(cmd *cobra.Command) bool {
	return cmderr.BackendFlagChanged(cmd)
}

// Package mecmd wires the `cr me` command surface.
package mecmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/open-cli-collective/cli-common/credstore"
	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmderr"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmdruntime"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	githubprovider "github.com/open-cli-collective/codereview-cli/internal/gitprovider/github"
	"github.com/open-cli-collective/codereview-cli/internal/identity"
	"github.com/open-cli-collective/codereview-cli/internal/progress"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

// IdentityResolverFactory builds the resolver used by `cr me`.
type IdentityResolverFactory func(cmd *cobra.Command, opts *root.Options, cfg config.File) (identity.Resolver, func(), error)

// Register attaches the me command to rootCmd.
func Register(rootCmd *cobra.Command, opts *root.Options) {
	RegisterWithFactory(rootCmd, opts, newGitHubResolver)
}

// RegisterWithFactory attaches the me command with an injected resolver factory.
func RegisterWithFactory(rootCmd *cobra.Command, opts *root.Options, factory IdentityResolverFactory) {
	var jsonOutput bool
	var all bool
	cmd := &cobra.Command{
		Use:   "me",
		Short: "Resolve and cache the active git-host identity",
		Args:  exitcode.NoArgs("me takes no arguments"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if all && opts.Profile != "" {
				return exitcode.Usage(fmt.Errorf("me --all cannot be combined with --profile"))
			}
			result, err := runMe(cmd.Context(), cmd, opts, factory, all)
			if err != nil {
				return err
			}
			return view.Render(opts.Stdout, jsonOutput, result, func(w io.Writer) error {
				return view.RenderMeText(w, result)
			})
		},
	}
	root.AddJSONFlag(cmd, &jsonOutput)
	cmd.Flags().BoolVar(&all, "all", false, "Refresh every configured profile")
	rootCmd.AddCommand(cmd)
}

func runMe(ctx context.Context, cmd *cobra.Command, opts *root.Options, factory IdentityResolverFactory, all bool) (view.MeResult, error) {
	return runMeWithSaver(ctx, cmd, opts, factory, all, config.Save)
}

func runMeWithSaver(ctx context.Context, cmd *cobra.Command, opts *root.Options, factory IdentityResolverFactory, all bool, saveConfig func(string, config.File) error) (view.MeResult, error) {
	path, err := cmdruntime.ConfigPath(opts)
	if err != nil {
		return view.MeResult{}, exitcode.AuthConfig(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		return view.MeResult{}, cmderr.Config(err)
	}
	if err := prevalidateIdentityProfiles(cfg, opts.Profile, all); err != nil {
		return view.MeResult{}, mapRunError(err)
	}
	resolver, cleanup, err := factory(cmd, opts, cfg)
	if err != nil {
		return view.MeResult{}, err
	}
	if cleanup != nil {
		defer cleanup()
	}

	var updated config.File
	var results []identity.ProfileResult
	var changed bool
	if all {
		updated, results, changed, err = identity.RefreshAll(ctx, cfg, resolver)
	} else {
		updated, results, changed, err = identity.Refresh(ctx, cfg, opts.Profile, resolver)
	}
	if err != nil {
		return view.MeResult{}, mapRunError(err)
	}
	if changed {
		if err := saveConfig(path, updated); err != nil {
			return view.MeResult{}, cmderr.Config(err)
		}
	}
	return view.NewMeResult(results), nil
}

func prevalidateIdentityProfiles(cfg config.File, profileName string, all bool) error {
	if !all {
		name, profile, err := config.ResolveProfile(cfg, profileName)
		if err != nil {
			return err
		}
		return prevalidatePostingIdentityProfile(name, profile)
	}
	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := prevalidateAllIdentityProfile(name, cfg.Profiles[name]); err != nil {
			return err
		}
	}
	return nil
}

func prevalidatePostingIdentityProfile(name string, profile config.Profile) error {
	if profile.ReviewerCredentials != nil {
		if !profile.ReviewerCredentials.AuthMode.Valid() {
			return fmt.Errorf("%w: profiles.%s.reviewer_credentials.auth_mode %q", config.ErrInvalid, name, profile.ReviewerCredentials.AuthMode)
		}
		if !profile.ReviewerCredentials.AuthMode.Supported() {
			return fmt.Errorf("%w: profiles.%s.reviewer_credentials.auth_mode %q", config.ErrUnsupported, name, profile.ReviewerCredentials.AuthMode)
		}
		return nil
	}
	if !profile.Git.AuthMode.Valid() {
		return fmt.Errorf("%w: profiles.%s.git.auth_mode %q", config.ErrInvalid, name, profile.Git.AuthMode)
	}
	if !profile.Git.AuthMode.Supported() {
		return fmt.Errorf("%w: profiles.%s.git.auth_mode %q", config.ErrUnsupported, name, profile.Git.AuthMode)
	}
	return nil
}

func prevalidateAllIdentityProfile(name string, profile config.Profile) error {
	if !profile.Git.AuthMode.Valid() {
		return fmt.Errorf("%w: profiles.%s.git.auth_mode %q", config.ErrInvalid, name, profile.Git.AuthMode)
	}
	if !profile.Git.AuthMode.Supported() {
		return fmt.Errorf("%w: profiles.%s.git.auth_mode %q", config.ErrUnsupported, name, profile.Git.AuthMode)
	}
	if profile.ReviewerCredentials != nil && !profile.ReviewerCredentials.AuthMode.Valid() {
		return fmt.Errorf("%w: profiles.%s.reviewer_credentials.auth_mode %q", config.ErrInvalid, name, profile.ReviewerCredentials.AuthMode)
	}
	if profile.ReviewerCredentials != nil && !profile.ReviewerCredentials.AuthMode.Supported() {
		return fmt.Errorf("%w: profiles.%s.reviewer_credentials.auth_mode %q", config.ErrUnsupported, name, profile.ReviewerCredentials.AuthMode)
	}
	return nil
}

func mapRunError(err error) error {
	switch {
	case errors.Is(err, config.ErrInvalid),
		errors.Is(err, config.ErrNotConfigured),
		errors.Is(err, config.ErrProfileNotFound),
		errors.Is(err, config.ErrSecretsStoreNotFound),
		errors.Is(err, config.ErrUnsupported):
		return cmderr.Config(err)
	case errors.Is(err, gitprovider.ErrAuth),
		errors.Is(err, gitprovider.ErrPermission),
		errors.Is(err, gitprovider.ErrRetryable),
		errors.Is(err, gitprovider.ErrNotFound),
		errors.Is(err, gitprovider.ErrConflict),
		errors.Is(err, gitprovider.ErrStaleSHA):
		return cmderr.Provider(err)
	// Keep this list in sync with cmderr.Credential. The explicit guard avoids
	// accidentally classifying arbitrary errors as credential-domain failures.
	case errors.Is(err, credentials.ErrInvalidBackendSelection),
		errors.Is(err, credentials.ErrWrongService),
		errors.Is(err, credstore.ErrRefEmpty),
		errors.Is(err, credstore.ErrRefSegmentCount),
		errors.Is(err, credstore.ErrRefInvalidChar),
		errors.Is(err, credstore.ErrKeyNotAllowed),
		errors.Is(err, credstore.ErrExists),
		errors.Is(err, credstore.ErrFilePassphraseRequired),
		errors.Is(err, credstore.ErrSecretServiceFailClosed),
		errors.Is(err, credstore.ErrStoreClosed),
		errors.Is(err, credstore.ErrBackendNotImplemented):
		return cmderr.Credential(err)
	}
	return err
}

func newGitHubResolver(cmd *cobra.Command, opts *root.Options, cfg config.File) (identity.Resolver, func(), error) {
	return &githubResolver{
		cfg:                cfg,
		backend:            opts.Backend,
		backendFlagChanged: cmderr.BackendFlagChanged(cmd),
		warnings:           opts.Stderr,
		logger:             root.NewProgressLogger(opts),
	}, nil, nil
}

type githubResolver struct {
	cfg                config.File
	backend            string
	backendFlagChanged bool
	options            githubprovider.Options
	warnings           io.Writer
	logger             *progress.Logger
	NewClient          func(config.GitConfig, credentials.Reader, githubprovider.Options) (*githubprovider.Client, gitprovider.Credential, error)
}

// ResolveIdentity resolves one configured GitHub identity.
func (r *githubResolver) ResolveIdentity(ctx context.Context, profileName string, git config.GitConfig) (gitprovider.Identity, error) {
	newClient := r.NewClient
	if newClient == nil {
		newClient = githubprovider.NewFromGitConfig
	}
	resolvedSecretsStore, err := credentials.ResolveSecretsStoreForRef(r.cfg, git.Credential.Name, profileName)
	if err != nil {
		return gitprovider.Identity{}, err
	}
	store, err := credentials.OpenResolvedStore(r.backend, r.backendFlagChanged, r.cfg, resolvedSecretsStore)
	if err != nil {
		return gitprovider.Identity{}, err
	}
	defer store.Close()
	options := r.options
	if installationID := config.PinnedGitHubAppInstallationIDForGit(r.cfg.Profiles[profileName], git); installationID != "" {
		options.InstallationID = installationID
	} else if git.AuthMode == config.GitAuthModeGitHubApp {
		cached := strings.TrimSpace(git.IdentityCache)
		if cached == "" {
			return gitprovider.Identity{}, fmt.Errorf("%w: profiles.%s github_app identity cannot be refreshed by cr me without a pinned installation or existing identity cache", config.ErrNotConfigured, profileName)
		}
		if r.warnings != nil {
			_, _ = fmt.Fprintf(r.warnings, "warning: profiles.%s github_app identity uses repository discovery and cannot be refreshed by cr me without PR context; preserving cached identity %q\n", profileName, cached)
		}
		return gitprovider.Identity{Login: cached}, nil
	}
	reader := credentials.ProgressStoreReader("me", r.logger, resolvedSecretsStore, store)
	client, credential, err := newClient(git, reader, options)
	if err != nil {
		return gitprovider.Identity{}, err
	}
	return client.WhoAmI(ctx, credential)
}

var _ identity.Resolver = (*githubResolver)(nil)

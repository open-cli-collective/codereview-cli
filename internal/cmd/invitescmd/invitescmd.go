// Package invitescmd wires the `cr invites` command surface.
package invitescmd

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmderr"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmdruntime"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/gitproviders"
	"github.com/open-cli-collective/codereview-cli/internal/invitations"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

// Provider is the provider surface `cr invites` needs: the identity lookup
// plus the account's repository invitations.
type Provider = gitproviders.InvitationClient

// ProviderFactory builds the provider authenticated as one profile's posting
// identity. The returned cleanup, when non-nil, closes the credential store.
type ProviderFactory func(cmd *cobra.Command, opts *root.Options, cfg config.File, profileName string, git config.GitConfig) (Provider, gitprovider.Credential, func(), error)

const (
	sourceGit      = "git"
	sourceReviewer = "reviewer_credentials"
)

const invitesLong = `List and accept the pending repository invitations addressed to the selected
profile's posting identity: its reviewer_credentials when the profile declares
them, otherwise its git credential. That is the identity cr review posts as.

An approval counts toward a required review only when the reviewer identity
has write access to the repository. Accepting the owner's invitation grants
that access using the token cr already holds; cr never prints or exports it.

Requires a GitHub personal access token. GitHub App credentials and GitLab
profiles are unsupported.`

const acceptLong = `Accept the pending invitations whose repository is on the allowlist built
from --owner and --repo. At least one is required, so accept never takes every
invitation by default.

Every pending invitation is reported: accepted, would_accept under --dry-run,
skipped with a reason (not on the allowlist, or expired), or failed with the
host's error. --dry-run accepts nothing. The command prints every result and
then exits 1 if any accept failed.`

// Register attaches the invites command tree to rootCmd.
func Register(rootCmd *cobra.Command, opts *root.Options) {
	RegisterWithFactory(rootCmd, opts, ConfiguredProviderFactory(gitproviders.Options{}))
}

// RegisterWithFactory attaches the invites command tree with an injected
// provider factory.
func RegisterWithFactory(rootCmd *cobra.Command, opts *root.Options, factory ProviderFactory) {
	cmd := &cobra.Command{
		Use:   "invites",
		Short: "List and accept repository invitations for the posting identity",
		Long:  invitesLong,
	}
	cmd.AddCommand(newListCommand(opts, factory), newAcceptCommand(opts, factory))
	rootCmd.AddCommand(cmd)
}

type allowlistFlags struct {
	owners []string
	repos  []string
}

func (f *allowlistFlags) add(cmd *cobra.Command, verb string) {
	cmd.Flags().StringArrayVar(&f.owners, "owner", nil, verb+" invitations to repositories owned by this login; repeat for multiple owners")
	cmd.Flags().StringArrayVar(&f.repos, "repo", nil, verb+" the invitation to this owner/name repository; repeat for multiple repos")
}

func newListCommand(opts *root.Options, factory ProviderFactory) *cobra.Command {
	var filter allowlistFlags
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List pending repository invitations",
		Long:  invitesLong,
		Example: `  cr invites list --profile reviewer
  cr invites list --profile reviewer --owner example-org --json`,
		Args: exitcode.NoArgs("invites list takes no arguments"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			allow, err := invitations.NewAllowlist(filter.owners, filter.repos)
			if err != nil {
				return exitcode.Usage(err)
			}
			session, err := openSession(cmd, opts, factory)
			if err != nil {
				return err
			}
			defer session.close()
			pending, err := invitations.List(cmd.Context(), session.provider, allow)
			if err != nil {
				return mapRunError(err)
			}
			result := view.NewInvitesList(session.identity, pending)
			return view.Render(opts.Stdout, jsonOutput, result, func(w io.Writer) error {
				return view.RenderInvitesListText(w, result)
			})
		},
	}
	filter.add(cmd, "Only list")
	root.AddJSONFlag(cmd, &jsonOutput)
	return cmd
}

func newAcceptCommand(opts *root.Options, factory ProviderFactory) *cobra.Command {
	var allowFlags allowlistFlags
	var dryRun bool
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "accept (--owner <login> | --repo <owner/name>)...",
		Short: "Accept allowlisted repository invitations",
		Long:  acceptLong,
		Example: `  cr invites accept --profile reviewer --owner example-org --dry-run
  cr invites accept --profile reviewer --owner example-org
  cr invites accept --profile reviewer --repo example-org/service --json`,
		Args: exitcode.NoArgs("invites accept takes no arguments; name invitations with --owner or --repo"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			allow, err := invitations.NewAllowlist(allowFlags.owners, allowFlags.repos)
			if err != nil {
				return exitcode.Usage(err)
			}
			if allow.Empty() {
				return exitcode.Usage(fmt.Errorf("invites accept requires at least one --owner or --repo"))
			}
			session, err := openSession(cmd, opts, factory)
			if err != nil {
				return err
			}
			defer session.close()
			outcomes, err := invitations.Accept(cmd.Context(), session.provider, allow, dryRun)
			if err != nil {
				return mapRunError(err)
			}
			result := view.NewInvitesAccept(session.identity, allow, dryRun, outcomes)
			if err := view.Render(opts.Stdout, jsonOutput, result, func(w io.Writer) error {
				return view.RenderInvitesAcceptText(w, result)
			}); err != nil {
				return err
			}
			if failed := result.Summary.Failed; failed > 0 {
				return exitcode.With(exitcode.Failure, fmt.Errorf("invites accept: %d of %d accept attempts failed", failed, failed+result.Summary.Accepted))
			}
			return nil
		},
	}
	allowFlags.add(cmd, "Accept")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report what would be accepted without accepting anything")
	root.AddJSONFlag(cmd, &jsonOutput)
	return cmd
}

type session struct {
	provider Provider
	identity view.InvitesIdentity
	cleanup  func()
}

func (s session) close() {
	if s.cleanup != nil {
		s.cleanup()
	}
}

// openSession resolves the profile's posting credential, builds the provider,
// and confirms the live identity before any invitation is read.
func openSession(cmd *cobra.Command, opts *root.Options, factory ProviderFactory) (session, error) {
	cfg, err := cmdruntime.LoadConfig(opts)
	if err != nil {
		return session{}, cmderr.Config(err)
	}
	name, profile, err := config.ResolveProfile(cfg, opts.Profile)
	if err != nil {
		return session{}, mapRunError(err)
	}
	source, git := postingGitConfig(profile)
	if err := prevalidate(name, source, git); err != nil {
		return session{}, mapRunError(err)
	}
	provider, credential, cleanup, err := factory(cmd, opts, cfg, name, git)
	if err != nil {
		return session{}, mapRunError(err)
	}
	opened := session{provider: provider, cleanup: cleanup}
	live, err := provider.WhoAmI(cmd.Context(), credential)
	if err != nil {
		opened.close()
		return session{}, mapRunError(err)
	}
	opened.identity = view.InvitesIdentity{
		Profile:          name,
		CredentialSource: source,
		Host:             git.Host,
		Login:            live.Login,
	}
	return opened, nil
}

// postingGitConfig selects the identity cr review posts as, matching cr me.
func postingGitConfig(profile config.Profile) (string, config.GitConfig) {
	if profile.ReviewerCredentials == nil {
		return sourceGit, profile.Git
	}
	return sourceReviewer, config.GitConfig{
		Provider:      profile.Git.Provider,
		Host:          profile.Git.Host,
		AuthMode:      profile.ReviewerCredentials.AuthMode,
		Credential:    profile.ReviewerCredentials.Credential,
		IdentityCache: profile.ReviewerCredentials.IdentityCache,
	}
}

// prevalidate rejects credentials that cannot hold repository invitations
// before the credential store is opened, so no keychain prompt is wasted.
func prevalidate(profileName, source string, git config.GitConfig) error {
	field := fmt.Sprintf("profiles.%s.%s.auth_mode", profileName, source)
	if !git.AuthMode.Valid() {
		return fmt.Errorf("%w: %s %q", config.ErrInvalid, field, git.AuthMode)
	}
	if git.AuthMode != config.GitAuthModePAT {
		return fmt.Errorf("%w: %s %q; repository invitations need a personal access token", config.ErrUnsupported, field, git.AuthMode)
	}
	if git.ProviderKind() == config.GitProviderGitLab {
		return fmt.Errorf("%w: repository invitations for provider gitlab; GitLab adds existing users as project members without an invitation to accept", config.ErrUnsupported)
	}
	return nil
}

func mapRunError(err error) error {
	if errors.Is(err, invitations.ErrInvalidAllowlist) || errors.Is(err, invitations.ErrAllowlistRequired) {
		return exitcode.Usage(err)
	}
	return cmderr.Classify(err)
}

// ConfiguredProviderFactory returns the production factory: it opens the
// profile's credential store and builds the host adapter with options.
func ConfiguredProviderFactory(options gitproviders.Options) ProviderFactory {
	return func(cmd *cobra.Command, opts *root.Options, cfg config.File, profileName string, git config.GitConfig) (Provider, gitprovider.Credential, func(), error) {
		return newConfiguredProvider(cmd, opts, cfg, profileName, git, options)
	}
}

func newConfiguredProvider(cmd *cobra.Command, opts *root.Options, cfg config.File, profileName string, git config.GitConfig, options gitproviders.Options) (Provider, gitprovider.Credential, func(), error) {
	resolved, err := credentials.ResolveSecretsStoreForRef(cfg, git.Credential.Name, profileName)
	if err != nil {
		return nil, gitprovider.Credential{}, nil, err
	}
	store, err := credentials.OpenResolvedStore(opts.Backend, cmderr.BackendFlagChanged(cmd), cfg, resolved)
	if err != nil {
		return nil, gitprovider.Credential{}, nil, err
	}
	reader := credentials.ProgressStoreReader("invites", root.NewProgressLogger(opts), resolved, store)
	client, credential, err := gitproviders.NewInvitationClient(git, reader, options)
	if err != nil {
		_ = store.Close()
		return nil, gitprovider.Credential{}, nil, err
	}
	return client, credential, func() { _ = store.Close() }, nil
}

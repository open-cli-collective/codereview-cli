// Package credentialcmd wires credential ingress commands.
package credentialcmd

import (
	"errors"
	"fmt"

	"github.com/open-cli-collective/cli-common/credstore"
	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmderr"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmdruntime"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

// Register attaches credential commands to rootCmd.
func Register(rootCmd *cobra.Command, opts *root.Options) {
	rootCmd.AddCommand(newSetCredentialCommand(opts))
}

type setCredentialOptions struct {
	ref       string
	store     string
	name      string
	key       string
	stdin     bool
	fromEnv   string
	overwrite bool
	json      bool
}

func newSetCredentialCommand(opts *root.Options) *cobra.Command {
	var flags setCredentialOptions
	cmd := &cobra.Command{
		Use:   "set-credential --store <id> --name <credential-name> --key <key>",
		Short: "Write one secret value to a credential store",
		Args:  exitcode.NoArgs("set-credential takes no arguments"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := runSetCredential(cmd, opts, flags)
			if flags.json {
				if err != nil {
					result.Written = false
					result.Error = err.Error()
				}
				if renderErr := view.RenderCredentialWriteJSON(opts.Stdout, result); renderErr != nil && err == nil {
					return renderErr
				}
			}
			return err
		},
	}
	cmd.Flags().StringVar(&flags.store, "store", "", "Credential store id")
	cmd.Flags().StringVar(&flags.name, "name", "", "Credential name (<service>/<profile>)")
	cmd.Flags().StringVar(&flags.key, "key", "", "Credential key")
	cmd.Flags().BoolVar(&flags.stdin, "stdin", false, "Read the secret value from stdin")
	cmd.Flags().StringVar(&flags.fromEnv, "from-env", "", "Read the secret value from this environment variable")
	cmd.Flags().BoolVar(&flags.overwrite, "overwrite", false, "Replace an existing credential")
	cmd.Flags().BoolVar(&flags.json, "json", false, "Emit JSON")
	cmd.Flags().StringVar(&flags.ref, "ref", "", "Deprecated compatibility flag; use --name")
	_ = cmd.Flags().MarkHidden("ref")
	return cmd
}

func runSetCredential(cmd *cobra.Command, opts *root.Options, flags setCredentialOptions) (view.CredentialWrite, error) {
	result := view.CredentialWrite{Store: flags.store, Name: flags.name, Key: flags.key}
	if flags.ref != "" {
		return result, exitcode.Usage(fmt.Errorf("--ref has been replaced by --name"))
	}
	if flags.store == "" {
		return result, exitcode.Usage(fmt.Errorf("--store is required"))
	}
	if flags.name == "" {
		return result, exitcode.Usage(fmt.Errorf("--name is required"))
	}
	parsed, err := credentials.ParseRef(flags.name)
	if err != nil {
		return result, exitcode.Usage(err)
	}
	if flags.key == "" {
		return result, exitcode.Usage(fmt.Errorf("--key is required"))
	}
	cfg, err := loadOptionalConfig(opts)
	if err != nil {
		return result, cmderr.Config(err)
	}
	if err := credentials.ValidateAllowedKeyForConfig(cfg, flags.name, flags.key); err != nil {
		if errors.Is(err, config.ErrInvalid) || errors.Is(err, config.ErrUnsupported) {
			return result, cmderr.Config(err)
		}
		return result, exitcode.Usage(err)
	}
	backendFlagChanged := cmderr.BackendFlagChanged(cmd)
	resolvedStore, err := credentials.ResolveCredentialStore(cfg, flags.store)
	if err != nil {
		if errors.Is(err, config.ErrInvalid) || errors.Is(err, config.ErrSecretsStoreNotFound) {
			return result, cmderr.Config(err)
		}
		return result, exitcode.AuthConfig(err)
	}
	store, err := credentials.OpenResolvedStore(opts.Backend, backendFlagChanged, cfg, resolvedStore)
	if err != nil {
		if config.IsConfigSelection(err) {
			return result, cmderr.Config(err)
		}
		return result, cmderr.Credential(err)
	}
	defer store.Close()
	secret, err := cmdruntime.ReadSecretIngress(opts.Stdin, flags.stdin, flags.fromEnv, "--stdin", "--from-env")
	if err != nil {
		return result, exitcode.Usage(err)
	}
	backend, _ := store.Backend()
	result.Backend = string(backend)
	result.BackendSource = string(credentials.BackendSourceCredentialStore)
	setOpts := []credstore.SetOpt{}
	if flags.overwrite {
		setOpts = append(setOpts, credstore.WithOverwrite())
	}
	if err := store.Set(parsed.Profile, flags.key, secret, setOpts...); err != nil {
		return result, cmderr.Credential(err)
	}
	result.Written = true
	if !flags.json {
		_, err = fmt.Fprintf(opts.Stderr, "wrote %s to %s in %s via %s\n", flags.key, flags.name, resolvedStore.DisplayName(), backend)
	}
	return result, err
}

func loadOptionalConfig(opts *root.Options) (config.File, error) {
	path, err := cmdruntime.ConfigPath(opts)
	if err != nil {
		return config.File{}, err
	}
	cfg, err := config.Load(path)
	if errors.Is(err, config.ErrNotConfigured) {
		return config.File{}, nil
	}
	return cfg, err
}

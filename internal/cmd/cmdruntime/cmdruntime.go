// Package cmdruntime contains shared command runtime helpers.
package cmdruntime

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/open-cli-collective/codereview-cli/internal/agents"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmderr"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
)

// ConfigPath resolves the active config path from root options.
func ConfigPath(opts *root.Options) (string, error) {
	if opts != nil && opts.ConfigPath != "" {
		return opts.ConfigPath, nil
	}
	return config.Path()
}

// ReadSecretIngress reads a required secret from stdin or an environment variable.
func ReadSecretIngress(r io.Reader, stdin bool, envVar, stdinFlag, envFlag string) (string, error) {
	value, ok, err := ReadOptionalSecretIngress(r, stdin, envVar, stdinFlag, envFlag)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("exactly one of %s or %s is required", stdinFlag, envFlag)
	}
	return value, nil
}

// ReadOptionalSecretIngress reads an optional secret from stdin or an environment variable.
func ReadOptionalSecretIngress(r io.Reader, stdin bool, envVar, stdinFlag, envFlag string) (string, bool, error) {
	if stdin && envVar != "" {
		return "", false, fmt.Errorf("only one of %s or %s may be set", stdinFlag, envFlag)
	}
	if !stdin && envVar == "" {
		return "", false, nil
	}
	var value string
	if stdin {
		bytes, err := io.ReadAll(r)
		if err != nil {
			return "", false, fmt.Errorf("read %s: %w", stdinFlag, err)
		}
		value = credentials.TrimSecretIngress(string(bytes))
	} else {
		value = os.Getenv(envVar)
	}
	if value == "" {
		return "", false, fmt.Errorf("%s supplied an empty secret", ingressName(stdin, envVar, stdinFlag, envFlag))
	}
	return value, true, nil
}

func ingressName(stdin bool, envVar, stdinFlag, envFlag string) string {
	if stdin {
		return stdinFlag
	}
	return fmt.Sprintf("%s %s", envFlag, envVar)
}

// MapRunError maps lower-level runtime errors to CLI exit-code wrappers.
func MapRunError(err error) error {
	mappedError := func(err error) (error, bool) {
		var coder interface{ ExitCode() int }
		return err, errors.As(err, &coder)
	}
	if mapped, ok := mappedError(cmderr.Config(err)); ok {
		return mapped
	}
	if errors.Is(err, agents.ErrUnsafeSource) {
		return exitcode.Usage(err)
	}
	if mapped, ok := mappedError(cmderr.Provider(err)); ok {
		return mapped
	}
	if mapped, ok := mappedError(cmderr.Credential(err)); ok {
		return mapped
	}
	return err
}

// MissingResponderError reports that a command runtime cannot execute response runs.
func MissingResponderError() error {
	return fmt.Errorf("respond: runtime responder is required")
}

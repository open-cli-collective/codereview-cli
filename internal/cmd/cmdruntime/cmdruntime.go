// Package cmdruntime contains shared command runtime helpers.
package cmdruntime

import (
	"errors"
	"fmt"

	"github.com/open-cli-collective/codereview-cli/internal/agents"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmderr"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
)

// ConfigPath resolves the active config path from root options.
func ConfigPath(opts *root.Options) (string, error) {
	if opts != nil && opts.ConfigPath != "" {
		return opts.ConfigPath, nil
	}
	return config.Path()
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

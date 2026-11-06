package benchmarkcmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentity"
)

func newReplayCommand(opts *root.Options) *cobra.Command {
	return &cobra.Command{
		Use:   "replay <fixture.json>",
		Short: "Replay recorded report-identity decisions offline",
		Args:  exitcode.ExactArgs(1, "benchmark replay requires one fixture path"),
		RunE: func(_ *cobra.Command, args []string) error {
			data, err := readReplayFixture(args[0])
			if err != nil {
				return exitcode.With(exitcode.Failure, fmt.Errorf("benchmark replay: read fixture: %w", err))
			}
			input, err := reportidentity.Decode(data)
			if err != nil {
				return exitcode.Usage(fmt.Errorf("benchmark replay: invalid fixture: %w", err))
			}
			result, err := reportidentity.Replay(input)
			if err != nil {
				return exitcode.Usage(fmt.Errorf("benchmark replay: invalid fixture: %w", err))
			}

			encoder := json.NewEncoder(opts.Stdout)
			encoder.SetEscapeHTML(false)
			if err := encoder.Encode(result); err != nil {
				return exitcode.With(exitcode.Failure, fmt.Errorf("benchmark replay: write result: %w", err))
			}
			if result.Status == "error" {
				return exitcode.With(exitcode.Failure, errors.New("benchmark replay: one or more recorded comparisons failed"))
			}
			return nil
		},
	}
}

func readReplayFixture(path string) ([]byte, error) {
	// #nosec G304 -- caller-selected local fixture is explicit command input, read bounded by MaxFixtureBytes.
	fixture, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(fixture, reportidentity.MaxFixtureBytes+1))
	closeErr := fixture.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return data, nil
}

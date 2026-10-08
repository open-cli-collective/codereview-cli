package root

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/modelcatalog"
	"github.com/open-cli-collective/codereview-cli/internal/version"
)

func TestNewCommandHelpAndVersion(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantOutput string
	}{
		{name: "no args", args: nil, wantOutput: "Usage:"},
		{name: "help flag", args: []string{"--help"}, wantOutput: "Usage:"},
		{name: "help command", args: []string{"help"}, wantOutput: "Usage:"},
		{name: "help command topic", args: []string{"help", "version"}, wantOutput: "Print the build version"},
		{name: "version flag", args: []string{"--version"}, wantOutput: "cr " + version.Info()},
		{name: "version command", args: []string{"version"}, wantOutput: "cr " + version.Info()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			cmd, _ := NewCommandWithOptions(&Options{
				Stdin:  strings.NewReader(""),
				Stdout: &out,
				Stderr: &out,
			})

			if err := Execute(cmd, tt.args); err != nil {
				t.Fatalf("Execute(%q): %v", tt.args, err)
			}
			if got := out.String(); !strings.Contains(got, tt.wantOutput) {
				t.Fatalf("Execute(%q) output = %q, want substring %q", tt.args, got, tt.wantOutput)
			}
		})
	}
}

func TestPersistentProfileFlagPopulatesOptions(t *testing.T) {
	var out bytes.Buffer
	cmd, opts := NewCommandWithOptions(&Options{
		Stdin:  strings.NewReader(""),
		Stdout: &out,
		Stderr: &out,
	})

	if err := Execute(cmd, []string{"--profile", "work", "version"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if opts.Profile != "work" {
		t.Fatalf("Profile = %q, want work", opts.Profile)
	}
}

func TestPersistentQuietFlagPopulatesOptions(t *testing.T) {
	var out bytes.Buffer
	cmd, opts := NewCommandWithOptions(&Options{
		Stdin:  strings.NewReader(""),
		Stdout: &out,
		Stderr: &out,
	})

	if err := Execute(cmd, []string{"--quiet", "version"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !opts.Quiet {
		t.Fatal("Quiet = false, want true")
	}
}

func TestCatalogSnapshotIsCachedForOneCommand(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	source := t.TempDir()
	dataDir := filepath.Join(filepath.Dir(testFile), "..", "..", "modelcatalog", "data")
	for _, name := range []string{"manifest.json", "runtimes.csv", "models.csv", "defaults.csv", "pricing.csv"} {
		body, err := os.ReadFile(filepath.Join(dataDir, name)) // #nosec G304 -- dataDir is the repository's bundled fixture.
		if err != nil {
			t.Fatalf("read catalog %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(source, name), body, 0o600); err != nil { // #nosec G703 -- source is under t.TempDir.
			t.Fatalf("write catalog %s: %v", name, err)
		}
	}

	opts := &Options{CatalogPath: source}
	first, err := opts.CatalogSnapshot()
	if err != nil {
		t.Fatalf("first CatalogSnapshot: %v", err)
	}
	if first.Source().Kind != "local" {
		t.Fatalf("first source kind = %q, want local", first.Source().Kind)
	}
	beforeSelection, reason := first.PriceForRequest("openai-api-key", "gpt-6.1-sol", "default", 200000)
	if reason != "" || beforeSelection.Price.ContextBand != "short" {
		t.Fatalf("original selection = %#v, %q", beforeSelection, reason)
	}
	pricingPath := filepath.Join(source, "pricing.csv")
	body, err := os.ReadFile(pricingPath) // #nosec G304 -- under t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	body = bytes.ReplaceAll(body, []byte("272001"), []byte("100001"))
	if err := os.WriteFile(pricingPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := first.Manifest()
	manifest.Files["pricing.csv"] = fmt.Sprintf("%x", sha256.Sum256(body))
	manifestBody, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "manifest.json"), manifestBody, 0o600); err != nil {
		t.Fatal(err)
	}
	fresh, err := modelcatalog.LoadPath(source)
	if err != nil {
		t.Fatal(err)
	}
	freshSelection, reason := fresh.PriceForRequest("openai-api-key", "gpt-6.1-sol", "default", 200000)
	if reason != "" || freshSelection.Price.ContextBand != "long" || fresh.Digest() == first.Digest() || fresh.Revision() != first.Revision() {
		t.Fatal("changed on-disk metadata did not produce a different exact snapshot")
	}
	if err := os.RemoveAll(source); err != nil {
		t.Fatalf("remove source after first load: %v", err)
	}
	second, err := opts.CatalogSnapshot()
	if err != nil {
		t.Fatalf("second CatalogSnapshot: %v", err)
	}
	if second != first {
		t.Fatalf("second snapshot pointer = %p, want cached %p", second, first)
	}
	afterSelection, reason := second.PriceForRequest("openai-api-key", "gpt-6.1-sol", "default", 200000)
	if reason != "" || !reflect.DeepEqual(afterSelection, beforeSelection) {
		t.Fatal("command cached selection changed after on-disk metadata mutation")
	}
	if second.Revision() != first.Revision() {
		t.Fatalf("second revision = %q, want %q", second.Revision(), first.Revision())
	}

	if _, err := modelcatalog.LoadPath(source); err == nil {
		t.Fatal("removed source unexpectedly loaded")
	}
}

func TestCompletionCommandIsNotExposed(t *testing.T) {
	cmd, _ := NewCommandWithOptions(nil)
	err := Execute(cmd, []string{"completion"})
	if err == nil {
		t.Fatal("Execute(completion) error = nil, want usage error")
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.UsageError)
	}
}

func TestOutputShapeFlagsDeferred(t *testing.T) {
	tests := [][]string{
		{"--json"},
		{"--verbose"},
	}

	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			cmd, _ := NewCommandWithOptions(nil)
			err := Execute(cmd, args)
			if err == nil {
				t.Fatalf("Execute(%q) error = nil, want usage error", args)
			}
			if got := exitcode.FromError(err); got != exitcode.UsageError {
				t.Fatalf("exit code = %d, want %d", got, exitcode.UsageError)
			}
		})
	}
}

func TestDashVIsNotVersion(t *testing.T) {
	cmd, _ := NewCommandWithOptions(nil)
	err := Execute(cmd, []string{"-v"})
	if err == nil {
		t.Fatal("Execute(-v) error = nil, want usage error")
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.UsageError)
	}
}

func TestRegisterAll(t *testing.T) {
	cmd, opts := NewCommandWithOptions(nil)
	var calls []string
	RegisterAll(cmd, opts,
		func(parent *cobra.Command, got *Options) {
			if parent != cmd || got != opts {
				t.Fatalf("registrar got (%p, %p), want (%p, %p)", parent, got, cmd, opts)
			}
			calls = append(calls, "one")
		},
		func(parent *cobra.Command, got *Options) {
			if parent != cmd || got != opts {
				t.Fatalf("registrar got (%p, %p), want (%p, %p)", parent, got, cmd, opts)
			}
			calls = append(calls, "two")
		},
	)

	if strings.Join(calls, ",") != "one,two" {
		t.Fatalf("calls = %v, want one,two", calls)
	}
}

func TestExecuteMapsCobraUsageErrors(t *testing.T) {
	tests := [][]string{
		{"bogus"},
		{"help", "bogus"},
		{"version", "extra"},
	}

	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			cmd, _ := NewCommandWithOptions(nil)
			err := Execute(cmd, args)
			if err == nil {
				t.Fatalf("Execute(%q) error = nil, want error", args)
			}
			if got := exitcode.FromError(err); got != exitcode.UsageError {
				t.Fatalf("exit code = %d, want %d", got, exitcode.UsageError)
			}
		})
	}
}

func TestUnknownCommandPreflightUsesCobraFind(t *testing.T) {
	cmd, _ := NewCommandWithOptions(nil)
	_, _, err := cmd.Find([]string{"bogus"})
	if err == nil {
		t.Fatal("Find(bogus) error = nil, want cobra unknown-command error")
	}

	got := Execute(cmd, []string{"bogus"})
	if got == nil {
		t.Fatal("Execute(bogus) error = nil, want usage error")
	}
	if got.Error() != err.Error() {
		t.Fatalf("Execute(bogus) error = %q, want Find error %q", got, err)
	}
	if code := exitcode.FromError(got); code != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d", code, exitcode.UsageError)
	}
}

func TestExecuteLeavesGenericCommandErrorsAsFailure(t *testing.T) {
	cmd, _ := NewCommandWithOptions(nil)
	cmd.AddCommand(&cobra.Command{
		Use: "boom",
		RunE: func(*cobra.Command, []string) error {
			return errors.New("handler failed")
		},
	})

	err := Execute(cmd, []string{"boom"})
	if err == nil {
		t.Fatal("Execute(boom) error = nil, want error")
	}
	if got := exitcode.FromError(err); got != exitcode.Failure {
		t.Fatalf("exit code = %d, want %d", got, exitcode.Failure)
	}
}

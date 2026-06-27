package configcmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/open-cli-collective/cli-common/credstore"
	"github.com/open-cli-collective/cli-common/statedirtest"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/open-cli-collective/codereview-cli/internal/agents"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

func TestConfigShowText(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"config", "show", "--profile", "home"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.String(), "Profile: home") {
		t.Fatalf("stdout = %q, want home profile", out.String())
	}
	if !strings.Contains(out.String(), "adapter-managed; not stored by cr") {
		t.Fatalf("stdout = %q, want adapter-managed LLM note", out.String())
	}
	if !strings.Contains(out.String(), "medium: claude-sonnet-4-6 (built_in)") {
		t.Fatalf("stdout = %q, want built-in model map", out.String())
	}
}

func TestConfigShowProfileFlag(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"config", "show", "--profile", "work"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.String(), "Profile: work") {
		t.Fatalf("stdout = %q, want work profile", out.String())
	}
	if !strings.Contains(out.String(), "Credential name: codereview/work-llm") {
		t.Fatalf("stdout = %q, want work LLM ref", out.String())
	}
}

func TestConfigShowProfileFlagLastValueWins(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"--profile", "home", "config", "show", "--profile", "work"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.String(), "Profile: work") {
		t.Fatalf("stdout = %q, want command-position profile to win", out.String())
	}
}

func TestConfigShowJSON(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"config", "show", "--profile", "work", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.ConfigShow
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.ActiveProfile != "work" {
		t.Fatalf("active_profile = %q, want work", got.ActiveProfile)
	}
	if got.LLMCredential.Ref != "codereview/work-llm" {
		t.Fatalf("llm credential = %#v, want work LLM ref", got.LLMCredential)
	}
	if got.Backend != "memory" || got.BackendSource != "credential_store" {
		t.Fatalf("backend = (%q,%q), want (memory,credential_store)", got.Backend, got.BackendSource)
	}
	if got.CredentialRef != "codereview/work" {
		t.Fatalf("credential_ref = %q, want codereview/work", got.CredentialRef)
	}
	if len(got.CredentialRefs) != 3 {
		t.Fatalf("credential_refs len = %d, want 3", len(got.CredentialRefs))
	}
	wantKeys := map[string]string{
		"git":                  credentials.GitTokenKey,
		"reviewer_credentials": credentials.GitTokenKey,
		"llm":                  credentials.AnthropicAPIKeyKey,
	}
	for _, ref := range got.CredentialRefs {
		wantKey, ok := wantKeys[ref.Purpose]
		if !ok {
			t.Fatalf("unexpected credential purpose %q in %#v", ref.Purpose, got.CredentialRefs)
		}
		if len(ref.Keys) != 1 || ref.Keys[0].Key != wantKey {
			t.Fatalf("credential keys for %s = %#v, want %s", ref.Purpose, ref.Keys, wantKey)
		}
		delete(wantKeys, ref.Purpose)
	}
	if len(wantKeys) != 0 {
		t.Fatalf("missing credential purposes: %#v", wantKeys)
	}
}

func TestKeychainProbeManifestMatchesConfigShowContract(t *testing.T) {
	manifest := readKeychainProbeManifest(t)
	wantCommand := []string{"config", "show", "--profile", "default", "--json"}
	if !reflect.DeepEqual(manifest.KeychainProbe.Command, wantCommand) {
		t.Fatalf("keychain probe command = %#v, want %#v", manifest.KeychainProbe.Command, wantCommand)
	}
	if manifest.KeychainProbe.SeedConfig.Content == "" {
		t.Fatal("keychain probe seed config content is empty")
	}
	if len(manifest.KeychainProbe.Assertions) == 0 {
		t.Fatal("keychain probe assertions are empty")
	}

	path := filepath.Join(t.TempDir(), "config.yml")
	writeRawConfig(t, path, manifest.KeychainProbe.SeedConfig.Content)

	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load(seed): %v", err)
	}
	profile, ok := loaded.Profiles["default"]
	if !ok {
		t.Fatalf("loaded profiles missing default: %#v", loaded.Profiles)
	}
	if profile.RepositoryAccess != "default-git" {
		t.Fatalf("default repository_access = %q, want default-git", profile.RepositoryAccess)
	}

	cmd, out := newTestCommand(path)
	if err := root.Execute(cmd, manifest.KeychainProbe.Command); err != nil {
		t.Fatalf("Execute keychain probe command: %v", err)
	}

	var got view.ConfigShow
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal keychain probe JSON: %v\n%s", err, out.String())
	}
	if got.Backend != "keychain" {
		if runtime.GOOS == "darwin" {
			t.Fatalf("backend = %q, want keychain", got.Backend)
		}
	}
	if got.CredentialRef != "codereview/default" {
		t.Fatalf("credential_ref = %q, want codereview/default", got.CredentialRef)
	}
	for key, want := range manifest.KeychainProbe.Assertions {
		gotValue, ok := configShowAssertionValue(got, key)
		if !ok {
			t.Fatalf("unsupported manifest assertion key %q", key)
		}
		if runtime.GOOS != "darwin" && (key == ".backend" || key == ".backend_source") {
			continue
		}
		if gotValue != want {
			t.Fatalf("assertion %s = %q, want %q", key, gotValue, want)
		}
	}
}

func TestConfigPathText(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"config", "path"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	wantPath := filepath.Clean(path)
	wantDir := filepath.Dir(wantPath)
	got := out.String()
	if !strings.Contains(got, "Config path: "+wantPath) {
		t.Fatalf("stdout = %q, want config path %q", got, wantPath)
	}
	if !strings.Contains(got, "Config dir: "+wantDir) {
		t.Fatalf("stdout = %q, want config dir %q", got, wantDir)
	}
}

func TestConfigPathJSON(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"config", "path", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.ConfigPath
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	wantPath := filepath.Clean(path)
	if got.ConfigPath != wantPath || got.ConfigDir != filepath.Dir(wantPath) {
		t.Fatalf("config path JSON = %#v, want path %q dir %q", got, wantPath, filepath.Dir(wantPath))
	}
}

func TestConfigPathUsesDefaultResolvedPath(t *testing.T) {
	statedirtest.Hermetic(t)
	expectedPath, err := config.Path()
	if err != nil {
		t.Fatalf("config.Path: %v", err)
	}
	cmd, out := newTestCommandWithOptions(&root.Options{})

	if err := root.Execute(cmd, []string{"config", "path", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.ConfigPath
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.ConfigPath != expectedPath || got.ConfigDir != filepath.Dir(expectedPath) {
		t.Fatalf("config path JSON = %#v, want path %q dir %q", got, expectedPath, filepath.Dir(expectedPath))
	}
}

func TestConfigPathTextUsesDefaultResolvedPathOffline(t *testing.T) {
	statedirtest.Hermetic(t)
	expectedPath, err := config.Path()
	if err != nil {
		t.Fatalf("config.Path: %v", err)
	}
	cmd, out := newTestCommandWithOptions(&root.Options{})

	if err := root.Execute(cmd, []string{"config", "path"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := "Config path: " + expectedPath + "\nConfig dir: " + filepath.Dir(expectedPath) + "\n"
	if out.String() != want {
		t.Fatalf("stdout = %q, want %q", out.String(), want)
	}
}

func TestConfigDefaultCommandIsRemoved(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	cmd, _ := newTestCommand(path)

	err := root.Execute(cmd, []string{"config", "default", "get"})
	if err == nil || !strings.Contains(err.Error(), `unknown config command "default"`) {
		t.Fatalf("Execute error = %v, want unknown command", err)
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.UsageError)
	}
}

func TestConfigCredentialStoreListTextIncludesBuiltIn(t *testing.T) {
	cfg := testConfig()
	path := saveTestConfig(t, cfg)
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"config", "credential-store", "list"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := "Credential stores:\n  - local-os: OS credential store (auto, built_in)\n  - test-memory: Test Memory Store (memory, configured)\n"
	if out.String() != want {
		t.Fatalf("stdout = %q, want %q", out.String(), want)
	}
}

func TestConfigCredentialStoreGetJSON(t *testing.T) {
	cfg := testConfig()
	cfg.Secrets.Stores["personal-keychain"] = config.SecretsStore{
		DisplayName: "Personal Keychain",
		Backend:     config.SecretsStoreBackend{Kind: "keychain"},
	}
	cfg.Secrets.Stores["work-file"] = config.SecretsStore{
		DisplayName: "Work File Store",
		Backend:     config.SecretsStoreBackend{Kind: "file"},
	}
	path := saveTestConfig(t, cfg)

	cmd, out := newTestCommand(path)
	if err := root.Execute(cmd, []string{"config", "credential-store", "get", "work-file", "--json"}); err != nil {
		t.Fatalf("Execute get: %v", err)
	}
	var got view.ConfigSecretsProfile
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal get JSON: %v\n%s", err, out.String())
	}
	want := view.ConfigSecretsProfile{ID: "work-file", Label: "Work File Store", Backend: "file", Source: "configured"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("get JSON = %#v, want %#v", got, want)
	}
}

func TestConfigCredentialStoreMutatingCommandsAreUnavailable(t *testing.T) {
	path := saveTestConfig(t, testConfig())

	for _, args := range [][]string{
		{"config", "credential-store", "set", "work-file", "--backend", "file"},
		{"config", "credential-store", "remove", "work-file"},
		{"config", "credential-store", "default", "get"},
		{"config", "secrets-profile", "list"},
	} {
		cmd, _ := newTestCommand(path)
		err := root.Execute(cmd, args)
		if err == nil {
			t.Fatalf("Execute(%v) error = nil, want unavailable command", args)
		}
		if got := exitcode.FromError(err); got != exitcode.UsageError {
			t.Fatalf("Execute(%v) exit code = %d, want %d; err=%v", args, got, exitcode.UsageError, err)
		}
	}
}

func TestConfigRouteListText(t *testing.T) {
	cfg := testConfig()
	cfg.RepositoryProfiles = []config.RepositoryProfile{
		{
			Profile: "work",
			Match: config.RepositoryProfileMatch{
				Host:      "https://GITHUB.com/",
				Namespace: "rianjs",
				Repos:     []string{"baz", "bar"},
			},
		},
		{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "open-cli-collective",
			},
		},
	}
	path := saveTestConfig(t, cfg)
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"config", "route", "list"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := "Routes:\n  - home: github.com/open-cli-collective\n  - work: github.com/rianjs [bar, baz]\n"
	if out.String() != want {
		t.Fatalf("stdout = %q, want %q", out.String(), want)
	}
}

func TestConfigRouteListJSON(t *testing.T) {
	cfg := testConfig()
	cfg.RepositoryProfiles = []config.RepositoryProfile{
		{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "open-cli-collective",
			},
		},
		{
			Profile: "work",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
				Repos:     []string{"bar", "baz"},
			},
		},
	}
	path := saveTestConfig(t, cfg)
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"config", "route", "list", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.ConfigRoutes
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	want := view.ConfigRoutes{
		Routes: []view.ConfigRoute{
			{Profile: "home", Host: "github.com", Namespace: "open-cli-collective"},
			{Profile: "work", Host: "github.com", Namespace: "rianjs", Repos: []string{"bar", "baz"}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("routes = %#v, want %#v", got, want)
	}
}

func TestConfigRouteSetNamespaceRouteUsesSelectedProfile(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"--profile", "home", "config", "route", "set", "--host", "https://github.com/", "--namespace", "open-cli-collective"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := out.String(); got != "Set route for profile home: github.com/open-cli-collective\n" {
		t.Fatalf("stdout = %q, want namespace route confirmation", got)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []config.RepositoryProfile{
		{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "open-cli-collective",
			},
		},
	}
	if !reflect.DeepEqual(cfg.RepositoryProfiles, want) {
		t.Fatalf("repository_profiles = %#v, want %#v", cfg.RepositoryProfiles, want)
	}
}

func TestConfigRouteSetRepoRoutesConvergesDeterministically(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"--profile", "work", "config", "route", "set", "--host", "github.com", "--namespace", "rianjs", "--repo", "baz", "--repo", "bar", "--repo", " bar "}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := out.String(); got != "Set route for profile work: github.com/rianjs [bar, baz]\n" {
		t.Fatalf("stdout = %q, want repo route confirmation", got)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []config.RepositoryProfile{
		{
			Profile: "work",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
				Repos:     []string{"bar", "baz"},
			},
		},
	}
	if !reflect.DeepEqual(cfg.RepositoryProfiles, want) {
		t.Fatalf("repository_profiles = %#v, want %#v", cfg.RepositoryProfiles, want)
	}
}

func TestConfigRouteSetMovesReposAcrossProfiles(t *testing.T) {
	cfg := testConfig()
	cfg.RepositoryProfiles = []config.RepositoryProfile{
		{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
				Repos:     []string{"bar", "baz"},
			},
		},
	}
	path := saveTestConfig(t, cfg)
	cmd, _ := newTestCommand(path)

	if err := root.Execute(cmd, []string{"--profile", "work", "config", "route", "set", "--host", "github.com", "--namespace", "rianjs", "--repo", "baz"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []config.RepositoryProfile{
		{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
				Repos:     []string{"bar"},
			},
		},
		{
			Profile: "work",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
				Repos:     []string{"baz"},
			},
		},
	}
	if !reflect.DeepEqual(loaded.RepositoryProfiles, want) {
		t.Fatalf("repository_profiles = %#v, want %#v", loaded.RepositoryProfiles, want)
	}
}

func TestConfigRouteSetPreservesSiblingNamespaceAndRepoRoutes(t *testing.T) {
	cfg := testConfig()
	cfg.RepositoryProfiles = []config.RepositoryProfile{
		{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
			},
		},
		{
			Profile: "work",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
				Repos:     []string{"bar"},
			},
		},
	}
	path := saveTestConfig(t, cfg)
	cmd, _ := newTestCommand(path)

	if err := root.Execute(cmd, []string{"--profile", "work", "config", "route", "set", "--host", "github.com", "--namespace", "rianjs", "--repo", "baz"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []config.RepositoryProfile{
		{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
			},
		},
		{
			Profile: "work",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
				Repos:     []string{"bar", "baz"},
			},
		},
	}
	if !reflect.DeepEqual(loaded.RepositoryProfiles, want) {
		t.Fatalf("repository_profiles = %#v, want %#v", loaded.RepositoryProfiles, want)
	}
}

func TestConfigRouteSetRejectsHostMismatch(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	cmd, _ := newTestCommand(path)

	err := root.Execute(cmd, []string{"--profile", "home", "config", "route", "set", "--host", "gitlab.com", "--namespace", "open-cli-collective"})
	if err == nil {
		t.Fatal("Execute error = nil, want usage error")
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.UsageError)
	}
}

func TestConfigRouteSetRejectsBlankRepo(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	cmd, _ := newTestCommand(path)

	err := root.Execute(cmd, []string{"--profile", "work", "config", "route", "set", "--host", "github.com", "--namespace", "rianjs", "--repo", " "})
	if err == nil {
		t.Fatal("Execute error = nil, want usage error")
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.UsageError)
	}
	if !strings.Contains(err.Error(), "--repo must be non-empty") {
		t.Fatalf("error = %q, want repo usage text", err)
	}
}

func TestConfigRouteUnsetNamespaceRoute(t *testing.T) {
	cfg := testConfig()
	cfg.RepositoryProfiles = []config.RepositoryProfile{
		{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "open-cli-collective",
			},
		},
		{
			Profile: "work",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
				Repos:     []string{"bar"},
			},
		},
	}
	path := saveTestConfig(t, cfg)
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"config", "route", "unset", "--host", "github.com", "--namespace", "open-cli-collective"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := out.String(); got != "Removed route: github.com/open-cli-collective\n" {
		t.Fatalf("stdout = %q, want removal confirmation", got)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []config.RepositoryProfile{
		{
			Profile: "work",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
				Repos:     []string{"bar"},
			},
		},
	}
	if !reflect.DeepEqual(loaded.RepositoryProfiles, want) {
		t.Fatalf("repository_profiles = %#v, want %#v", loaded.RepositoryProfiles, want)
	}
}

func TestConfigRouteUnsetRepoRoutesPrunesEmptyEntry(t *testing.T) {
	cfg := testConfig()
	cfg.RepositoryProfiles = []config.RepositoryProfile{
		{
			Profile: "work",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
				Repos:     []string{"bar", "baz"},
			},
		},
	}
	path := saveTestConfig(t, cfg)
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"config", "route", "unset", "--host", "github.com", "--namespace", "rianjs", "--repo", "bar", "--repo", "baz"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := out.String(); got != "Removed route: github.com/rianjs [bar, baz]\n" {
		t.Fatalf("stdout = %q, want repo removal confirmation", got)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.RepositoryProfiles) != 0 {
		t.Fatalf("repository_profiles = %#v, want empty after pruning", loaded.RepositoryProfiles)
	}
}

func TestConfigRouteUnsetPreservesSiblingNamespaceAndRepoRoutes(t *testing.T) {
	cfg := testConfig()
	cfg.RepositoryProfiles = []config.RepositoryProfile{
		{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
			},
		},
		{
			Profile: "work",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
				Repos:     []string{"bar", "baz"},
			},
		},
	}
	path := saveTestConfig(t, cfg)
	cmd, _ := newTestCommand(path)

	if err := root.Execute(cmd, []string{"config", "route", "unset", "--host", "github.com", "--namespace", "rianjs", "--repo", "baz"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []config.RepositoryProfile{
		{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
			},
		},
		{
			Profile: "work",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
				Repos:     []string{"bar"},
			},
		},
	}
	if !reflect.DeepEqual(loaded.RepositoryProfiles, want) {
		t.Fatalf("repository_profiles = %#v, want %#v", loaded.RepositoryProfiles, want)
	}
}

func TestConfigRouteUnsetAlreadyAbsentIsIdempotent(t *testing.T) {
	cfg := testConfig()
	cfg.RepositoryProfiles = []config.RepositoryProfile{
		{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "open-cli-collective",
			},
		},
	}
	path := saveTestConfig(t, cfg)
	// #nosec G304 -- test path is controlled by t.TempDir via saveTestConfig.
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile before: %v", err)
	}
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"config", "route", "unset", "--host", "github.com", "--namespace", "rianjs", "--repo", "bar"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := out.String(); got != "Route already absent: github.com/rianjs [bar]\n" {
		t.Fatalf("stdout = %q, want idempotent absence confirmation", got)
	}
	// #nosec G304 -- test path is controlled by t.TempDir via saveTestConfig.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("config changed during idempotent unset\nbefore:\n%s\nafter:\n%s", before, after)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(loaded.RepositoryProfiles, cfg.RepositoryProfiles) {
		t.Fatalf("repository_profiles = %#v, want unchanged %#v", loaded.RepositoryProfiles, cfg.RepositoryProfiles)
	}
}

func TestConfigRouteUnsetRejectsBlankInputs(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	tests := [][]string{
		{"config", "route", "unset", "--host", " ", "--namespace", "rianjs"},
		{"config", "route", "unset", "--host", "github.com", "--namespace", " "},
		{"config", "route", "unset", "--host", "github.com", "--namespace", "rianjs", "--repo", " "},
	}
	for _, args := range tests {
		cmd, _ := newTestCommand(path)
		err := root.Execute(cmd, args)
		if err == nil {
			t.Fatalf("Execute(%v) error = nil, want usage error", args)
		}
		if got := exitcode.FromError(err); got != exitcode.UsageError {
			t.Fatalf("Execute(%v) exit code = %d, want %d", args, got, exitcode.UsageError)
		}
	}
}

func TestConfigResolveProfileText(t *testing.T) {
	cfg := testConfig()
	cfg.RepositoryProfiles = []config.RepositoryProfile{
		{
			Profile: "work",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "open-cli-collective",
				Repos:     []string{"codereview-cli"},
			},
		},
	}
	path := saveTestConfig(t, cfg)
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"config", "resolve-profile", "https://github.com/open-cli-collective/codereview-cli/pull/1"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"PR URL: https://github.com/open-cli-collective/codereview-cli/pull/1",
		"Resolved profile: work",
		"Source: repository_route",
		"Matched route: github.com/open-cli-collective [codereview-cli]",
		"Git host: github.com",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stdout missing %q:\n%s", want, got)
		}
	}
}

func TestConfigResolveProfileRejectsUnmatchedRoute(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	cmd, _ := newTestCommand(path)

	err := root.Execute(cmd, []string{"config", "resolve-profile", "https://github.com/open-cli-collective/codereview-cli/pull/1", "--json"})
	if !errors.Is(err, config.ErrProfileNotFound) {
		t.Fatalf("Execute error = %v, want ErrProfileNotFound", err)
	}
	if got := exitcode.FromError(err); got != exitcode.AuthConfigError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.AuthConfigError)
	}
	if !strings.Contains(err.Error(), "no repository profile route matched github.com/open-cli-collective/codereview-cli") {
		t.Fatalf("error = %v, want unmatched route guidance", err)
	}
}

func TestConfigResolveProfileJSONIncludesMatchedRoute(t *testing.T) {
	cfg := testConfig()
	cfg.RepositoryProfiles = []config.RepositoryProfile{
		{
			Profile: "work",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "open-cli-collective",
				Repos:     []string{"codereview-cli"},
			},
		},
	}
	path := saveTestConfig(t, cfg)
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"config", "resolve-profile", "https://github.com/open-cli-collective/codereview-cli/pull/1", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.ConfigResolveProfile
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	wantRoute := &view.ConfigRoute{
		Profile:   "work",
		Host:      "github.com",
		Namespace: "open-cli-collective",
		Repos:     []string{"codereview-cli"},
	}
	if got.ResolvedProfile != "work" || got.Source != "repository_route" || got.GitHost != "github.com" || !reflect.DeepEqual(got.MatchedRoute, wantRoute) {
		t.Fatalf("resolve-profile JSON = %#v, want routed preview with matched route %#v", got, wantRoute)
	}
}

func TestConfigResolveProfileExplicitProfileBypassesRoute(t *testing.T) {
	cfg := testConfig()
	cfg.RepositoryProfiles = []config.RepositoryProfile{
		{
			Profile: "work",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "open-cli-collective",
				Repos:     []string{"codereview-cli"},
			},
		},
	}
	path := saveTestConfig(t, cfg)
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"--profile", "home", "config", "resolve-profile", "https://github.com/open-cli-collective/codereview-cli/pull/1", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.ConfigResolveProfile
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.ResolvedProfile != "home" || got.Source != "explicit_profile" || got.MatchedRoute != nil {
		t.Fatalf("resolve-profile JSON = %#v, want explicit bypass", got)
	}
}

func TestConfigResolveProfileRejectsHostMismatchAfterResolution(t *testing.T) {
	cfg := testConfig()
	home := cfg.Profiles["home"]
	home.Git.Host = "gitlab.com"
	cfg.Profiles["home"] = home
	path := saveTestConfig(t, cfg)
	cmd, _ := newTestCommand(path)

	err := root.Execute(cmd, []string{"--profile", "home", "config", "resolve-profile", "https://github.com/open-cli-collective/codereview-cli/pull/1"})
	if err == nil {
		t.Fatal("Execute error = nil, want usage error")
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.UsageError)
	}
}

func TestConfigResolveProfileRejectsHostMismatchForExplicitProfile(t *testing.T) {
	cfg := testConfig()
	work := cfg.Profiles["work"]
	work.Git.Host = "gitlab.com"
	cfg.Profiles["work"] = work
	path := saveTestConfig(t, cfg)
	cmd, _ := newTestCommand(path)

	err := root.Execute(cmd, []string{"--profile", "work", "config", "resolve-profile", "https://github.com/open-cli-collective/codereview-cli/pull/1"})
	if err == nil {
		t.Fatal("Execute error = nil, want usage error")
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.UsageError)
	}
}

func TestConfigResolveProfileExplicitEmptyProfileRejectsMissingSelection(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	cmd, _ := newTestCommand(path)

	err := root.Execute(cmd, []string{"--profile", "", "config", "resolve-profile", "https://github.com/open-cli-collective/codereview-cli/pull/1", "--json"})
	if !errors.Is(err, config.ErrProfileNotFound) {
		t.Fatalf("Execute error = %v, want ErrProfileNotFound", err)
	}
	if got := exitcode.FromError(err); got != exitcode.AuthConfigError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.AuthConfigError)
	}
	if !strings.Contains(err.Error(), "no profile selected") {
		t.Fatalf("error = %v, want no-profile-selected guidance", err)
	}
}

func TestConfigResolveProfileRejectsInvalidPRURL(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	cmd, _ := newTestCommand(path)

	err := root.Execute(cmd, []string{"config", "resolve-profile", "not-a-pr"})
	if err == nil {
		t.Fatal("Execute error = nil, want usage error")
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.UsageError)
	}
}

func TestConfigShowGitHubAppGitCredentialStatus(t *testing.T) {
	cfg := testConfig()
	work := cfg.Profiles["work"]
	work.Git.AuthMode = config.GitAuthModeGitHubApp
	work.Git.GitHubApp = &config.GitHubAppConfig{AppID: "12345"}
	work.Git.CredentialRef = "codereview/work-app"
	work.Git.Credential.Name = "codereview/work-app"
	cfg.Profiles["work"] = work
	path := saveTestConfig(t, cfg)
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"config", "show", "--profile", "work", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.ConfigShow
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	var gitStatus view.CredentialStatus
	for _, ref := range got.CredentialRefs {
		if ref.Purpose == "git" {
			gitStatus = ref
			break
		}
	}
	missing := false
	want := []view.KeyStatus{
		{Key: credentials.GitHubAppPrivateKeyKey, Required: true, Present: &missing, Status: "missing"},
	}
	if gitStatus.Ref != "codereview/work-app" || gitStatus.Mode != "github_app" || !reflect.DeepEqual(gitStatus.Keys, want) {
		t.Fatalf("git credential status = %#v, want app keys %#v", gitStatus, want)
	}
	if strings.Contains(out.String(), "private-key-value") || strings.Contains(out.String(), "installation-token") {
		t.Fatalf("config show leaked app secret material: %s", out.String())
	}
}

func TestConfigShowJSONReportsAgentSourceDeploymentStatus(t *testing.T) {
	available := t.TempDir()
	writeConfigTestAgentSource(t, available, "Do not inline this prompt.\n")
	missing := filepath.Join(t.TempDir(), "missing-agents")
	notDir := filepath.Join(t.TempDir(), "agent-source-file")
	if err := os.WriteFile(notDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile notDir: %v", err)
	}
	cfg := testConfig()
	home := cfg.Profiles["home"]
	home.AgentSources = []string{available, missing, notDir}
	cfg.Profiles["home"] = home
	path := saveTestConfig(t, cfg)
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"--profile", "home", "config", "show", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out.String(), "Do not inline this prompt") {
		t.Fatalf("config show inlined prompt contents: %s", out.String())
	}
	var got view.ConfigShow
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if len(got.AgentSources) != 3 {
		t.Fatalf("agent_sources len = %d, want 3: %#v", len(got.AgentSources), got.AgentSources)
	}
	first := got.AgentSources[0]
	if first.Status != agents.SourceStatusAvailable || !first.Present || first.Fingerprint == "" || first.CanonicalPath == "" {
		t.Fatalf("first source = %#v, want available fingerprinted source", first)
	}
	if !hasConfigSourceWarning(first.Warnings, "OS temp") {
		t.Fatalf("first source warnings = %#v, want nonfatal unsafe-source warning", first.Warnings)
	}
	second := got.AgentSources[1]
	if second.Status != agents.SourceStatusMissing || second.Present || second.Error == "" {
		t.Fatalf("second source = %#v, want missing non-fatal source", second)
	}
	third := got.AgentSources[2]
	if third.Status != agents.SourceStatusUnreadable || !third.Present || third.Error == "" {
		t.Fatalf("third source = %#v, want unreadable non-fatal source", third)
	}
}

func hasConfigSourceWarning(warnings []string, want string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, want) {
			return true
		}
	}
	return false
}

func TestConfigShowReportsUnknownPresenceWhenCredentialStoreCannotBeQueried(t *testing.T) {
	statedirtest.Hermetic(t)
	t.Setenv("CODEREVIEW_KEYRING_PASSPHRASE", "")
	cfg := testConfig()
	cfg.Secrets.Stores[testFileCredentialStoreID] = config.SecretsStore{
		DisplayName: "Test File Store",
		Backend:     config.SecretsStoreBackend{Kind: config.SecretsBackendKind(credstore.BackendFile)},
	}
	cfg = withCredentialStore(cfg, testFileCredentialStoreID)
	path := saveTestConfig(t, cfg)
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"--profile", "home", "config", "show", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.ConfigShow
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.Backend != "file" || got.BackendSource != "credential_store" {
		t.Fatalf("backend = (%q,%q), want (file,credential_store)", got.Backend, got.BackendSource)
	}
	if len(got.CredentialRefs) != 1 || len(got.CredentialRefs[0].Keys) != 1 {
		t.Fatalf("credential refs = %#v, want one key status", got.CredentialRefs)
	}
	key := got.CredentialRefs[0].Keys[0]
	if key.Status != "unknown" || key.Present != nil || key.Error == "" {
		t.Fatalf("key status = %#v, want unknown with error and no present bool", key)
	}
}

func TestConfigShowReportsEffectiveCredentialStores(t *testing.T) {
	cfg := testConfig()
	path := saveTestConfig(t, cfg)
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"--profile", "home", "config", "show", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.ConfigShow
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.Backend != "memory" || got.BackendSource != "credential_store" {
		t.Fatalf("backend = (%q,%q), want (memory,credential_store)", got.Backend, got.BackendSource)
	}
	want := []config.EffectiveSecretsProfile{{
		ID:          config.LocalOSCredentialStoreID,
		DisplayName: "OS credential store",
		Label:       "OS credential store",
		Backend:     config.ProjectedOSCredentialStoreBackendKind,
		ReadOnly:    true,
		Source:      config.EffectiveSecretsStoreSourceBuiltIn,
	}, {
		ID:          "test-memory",
		DisplayName: "Test Memory Store",
		Label:       "Test Memory Store",
		Backend:     "memory",
		Source:      config.EffectiveSecretsStoreSourceConfigured,
	}}
	if !reflect.DeepEqual(got.SecretsProfiles, want) {
		t.Fatalf("credential stores = %#v, want %#v", got.SecretsProfiles, want)
	}
}

func TestConfigShowSelectsProfileCredentialStore(t *testing.T) {
	cfg := testConfig()
	cfg.Secrets.Stores["personal-keychain"] = config.SecretsStore{
		DisplayName: "Personal Keychain",
		Backend:     config.SecretsStoreBackend{Kind: config.SecretsBackendKind("keychain")},
	}
	cfg.Secrets.Stores["work-file"] = config.SecretsStore{
		DisplayName: "Work File Store",
		Backend:     config.SecretsStoreBackend{Kind: config.SecretsBackendKind("file")},
	}
	cfg = withCredentialStore(cfg, "work-file")
	path := saveTestConfig(t, cfg)
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"--profile", "home", "config", "show", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.ConfigShow
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.Backend != "file" || got.BackendSource != "credential_store" {
		t.Fatalf("backend = (%q,%q), want selected credential-store backend (file,credential_store)", got.Backend, got.BackendSource)
	}
	if got.ActiveSecretsProfile == nil || got.ActiveSecretsProfile.ID != "work-file" || got.ActiveSecretsProfile.Label != "Work File Store" {
		t.Fatalf("active credential store = %#v, want work-file", got.ActiveSecretsProfile)
	}
	want := []config.EffectiveSecretsProfile{
		{
			ID:          config.LocalOSCredentialStoreID,
			DisplayName: "OS credential store",
			Label:       "OS credential store",
			Backend:     config.ProjectedOSCredentialStoreBackendKind,
			ReadOnly:    true,
			Source:      config.EffectiveSecretsStoreSourceBuiltIn,
		},
		{
			ID:          "personal-keychain",
			DisplayName: "Personal Keychain",
			Label:       "Personal Keychain",
			Backend:     "keychain",
			Source:      config.EffectiveSecretsProfileSourceConfigured,
		},
		{
			ID:          "test-memory",
			DisplayName: "Test Memory Store",
			Label:       "Test Memory Store",
			Backend:     "memory",
			Source:      config.EffectiveSecretsProfileSourceConfigured,
		},
		{
			ID:          "work-file",
			DisplayName: "Work File Store",
			Label:       "Work File Store",
			Backend:     "file",
			Source:      config.EffectiveSecretsProfileSourceConfigured,
		},
	}
	if !reflect.DeepEqual(got.SecretsProfiles, want) {
		t.Fatalf("credential stores = %#v, want %#v", got.SecretsProfiles, want)
	}
}

func TestConfigShowRejectsBackendOverrideForCredentialStore(t *testing.T) {
	cfg := testConfig()
	cfg.Secrets.Stores["work-file"] = config.SecretsStore{
		DisplayName: "Work File Store",
		Backend:     config.SecretsStoreBackend{Kind: config.SecretsBackendKind("file")},
	}
	cfg = withCredentialStore(cfg, "work-file")
	path := saveTestConfig(t, cfg)
	cmd, _ := newTestCommand(path)

	err := root.Execute(cmd, []string{"--backend", "memory", "--profile", "home", "config", "show", "--json"})
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("Execute error = %v, want ErrInvalid", err)
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.UsageError)
	}
}

func TestConfigShowOpenAIAPIKeyStatus(t *testing.T) {
	cfg := fileBackendConfig(t)
	work := cfg.Profiles["work"]
	work.LLM.Provider = config.LLMProviderOpenAI
	work.LLM.Adapter = config.LLMAdapterOpenAIAPI
	cfg.Profiles["work"] = work
	path := saveTestConfig(t, cfg)
	seedFileBackend(t, "work-llm", map[string]string{credentials.OpenAIAPIKeyKey: "openai-token"})
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"config", "show", "--profile", "work", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.ConfigShow
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "openai-token") {
		t.Fatalf("config show leaked API key: %q", out.String())
	}
	for _, ref := range got.CredentialRefs {
		if ref.Purpose != "llm" {
			continue
		}
		if len(ref.Keys) != 1 || ref.Keys[0].Key != credentials.OpenAIAPIKeyKey || ref.Keys[0].Present == nil || !*ref.Keys[0].Present {
			t.Fatalf("OpenAI LLM key status = %#v, want present %s", ref.Keys, credentials.OpenAIAPIKeyKey)
		}
		return
	}
	t.Fatalf("credential refs = %#v, want llm ref", got.CredentialRefs)
}

func TestConfigAgentSourceListText(t *testing.T) {
	cfg := testConfig()
	home := cfg.Profiles["home"]
	home.AgentSources = []string{"~/agents", "../shared/agents"}
	cfg.Profiles["home"] = home
	path := saveTestConfig(t, cfg)
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"--profile", "home", "config", "agent-source", "list"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := "Profile: home\nAgent sources:\n  - ~/agents\n  - ../shared/agents\n"
	if out.String() != want {
		t.Fatalf("stdout = %q, want %q", out.String(), want)
	}
}

func TestConfigAgentSourceListJSON(t *testing.T) {
	cfg := testConfig()
	work := cfg.Profiles["work"]
	work.AgentSources = []string{"./agents"}
	cfg.Profiles["work"] = work
	path := saveTestConfig(t, cfg)
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"--profile", "work", "config", "agent-source", "list", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.ConfigAgentSources
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	want := view.ConfigAgentSources{ActiveProfile: "work", AgentSources: []string{"./agents"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("agent-source list JSON = %#v, want %#v", got, want)
	}
}

func TestConfigAgentSourceListJSONEmptyArray(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"--profile", "home", "config", "agent-source", "list", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.ConfigAgentSources
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.ActiveProfile != "home" || got.AgentSources == nil || len(got.AgentSources) != 0 {
		t.Fatalf("agent-source list JSON = %#v, want empty array for home profile", got)
	}
}

func TestConfigAgentSourceAddNormalizesAndIsIdempotent(t *testing.T) {
	cfg := testConfig()
	home := cfg.Profiles["home"]
	home.AgentSources = []string{" ./agents/../agents/team/ "}
	cfg.Profiles["home"] = home
	path := saveTestConfig(t, cfg)

	cmd, out := newTestCommand(path)
	if err := root.Execute(cmd, []string{"--profile", "home", "config", "agent-source", "add", " ./agents/../agents/team/ "}); err != nil {
		t.Fatalf("Execute add: %v", err)
	}
	want := "Profile: home\nAgent sources:\n  -  ./agents/../agents/team/ \n"
	if out.String() != want {
		t.Fatalf("stdout after add = %q, want %q", out.String(), want)
	}

	cmd, out = newTestCommand(path)
	if err := root.Execute(cmd, []string{"--profile", "home", "config", "agent-source", "add", "./agents/../agents/team"}); err != nil {
		t.Fatalf("Execute second add: %v", err)
	}
	if out.String() != want {
		t.Fatalf("stdout after second add = %q, want %q", out.String(), want)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.Profiles["home"].AgentSources
	if !reflect.DeepEqual(got, []string{" ./agents/../agents/team/ "}) {
		t.Fatalf("agent_sources = %#v, want one preserved existing entry", got)
	}
}

func TestConfigAgentSourceRemoveIsIdempotent(t *testing.T) {
	cfg := testConfig()
	home := cfg.Profiles["home"]
	home.AgentSources = []string{"agents/team", "../shared/agents"}
	cfg.Profiles["home"] = home
	path := saveTestConfig(t, cfg)

	cmd, out := newTestCommand(path)
	if err := root.Execute(cmd, []string{"--profile", "home", "config", "agent-source", "remove", " ./agents/../agents/team "}); err != nil {
		t.Fatalf("Execute remove: %v", err)
	}
	want := "Profile: home\nAgent sources:\n  - ../shared/agents\n"
	if out.String() != want {
		t.Fatalf("stdout after remove = %q, want %q", out.String(), want)
	}

	cmd, out = newTestCommand(path)
	if err := root.Execute(cmd, []string{"--profile", "home", "config", "agent-source", "remove", "./missing"}); err != nil {
		t.Fatalf("Execute second remove: %v", err)
	}
	if out.String() != want {
		t.Fatalf("stdout after absent remove = %q, want %q", out.String(), want)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.Profiles["home"].AgentSources
	if !reflect.DeepEqual(got, []string{"../shared/agents"}) {
		t.Fatalf("agent_sources = %#v, want remaining source preserved", got)
	}
}

func TestConfigAgentSourceMutatesSelectedProfileOnly(t *testing.T) {
	cfg := testConfig()
	home := cfg.Profiles["home"]
	home.AgentSources = []string{"home-agents"}
	cfg.Profiles["home"] = home
	work := cfg.Profiles["work"]
	work.AgentSources = []string{"work-agents"}
	cfg.Profiles["work"] = work
	path := saveTestConfig(t, cfg)
	cmd, _ := newTestCommand(path)

	if err := root.Execute(cmd, []string{"--profile", "work", "config", "agent-source", "add", "./team/agents"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(cfg.Profiles["home"].AgentSources, []string{"home-agents"}) {
		t.Fatalf("home agent_sources = %#v, want unchanged", cfg.Profiles["home"].AgentSources)
	}
	if !reflect.DeepEqual(cfg.Profiles["work"].AgentSources, []string{"work-agents", "team/agents"}) {
		t.Fatalf("work agent_sources = %#v, want appended normalized source", cfg.Profiles["work"].AgentSources)
	}
}

func TestConfigAgentSourceRemoveMutatesSelectedProfileOnly(t *testing.T) {
	cfg := testConfig()
	home := cfg.Profiles["home"]
	home.AgentSources = []string{"home-agents"}
	cfg.Profiles["home"] = home
	work := cfg.Profiles["work"]
	work.AgentSources = []string{" ./team/agents/ ", "work-extra"}
	cfg.Profiles["work"] = work
	path := saveTestConfig(t, cfg)
	cmd, _ := newTestCommand(path)

	if err := root.Execute(cmd, []string{"--profile", "work", "config", "agent-source", "remove", "./team/agents"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(cfg.Profiles["home"].AgentSources, []string{"home-agents"}) {
		t.Fatalf("home agent_sources = %#v, want unchanged", cfg.Profiles["home"].AgentSources)
	}
	if !reflect.DeepEqual(cfg.Profiles["work"].AgentSources, []string{"work-extra"}) {
		t.Fatalf("work agent_sources = %#v, want normalized match removed only from selected profile", cfg.Profiles["work"].AgentSources)
	}
}

func TestConfigAgentSourcePreservesUnrelatedProfileFields(t *testing.T) {
	cfg := testConfig()
	home := cfg.Profiles["home"]
	home.AgentSources = []string{"home-agents"}
	cfg.Profiles["home"] = home
	path := saveTestConfig(t, cfg)
	cmd, _ := newTestCommand(path)
	want, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load baseline: %v", err)
	}

	if err := root.Execute(cmd, []string{"--profile", "home", "config", "agent-source", "add", "./team/agents"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	cfg, err = config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	wantHome := want.Profiles["home"]
	wantHome.AgentSources = []string{"home-agents", "team/agents"}
	want.Profiles["home"] = wantHome
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("config changed unexpectedly:\n got %#v\nwant %#v", cfg, want)
	}
}

func TestConfigAgentSourceAddRejectsBlankPath(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	cmd, _ := newTestCommand(path)

	err := root.Execute(cmd, []string{"--profile", "home", "config", "agent-source", "add", "   "})
	if err == nil {
		t.Fatal("Execute error = nil, want usage error")
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.UsageError)
	}
	if !strings.Contains(err.Error(), "path must be non-empty") {
		t.Fatalf("error = %q, want path usage text", err)
	}
}

func TestConfigAgentSourceRemoveRejectsBlankPath(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	cmd, _ := newTestCommand(path)

	err := root.Execute(cmd, []string{"--profile", "home", "config", "agent-source", "remove", "   "})
	if err == nil {
		t.Fatal("Execute error = nil, want usage error")
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.UsageError)
	}
	if !strings.Contains(err.Error(), "path must be non-empty") {
		t.Fatalf("error = %q, want path usage text", err)
	}
}

func TestConfigRetentionGetTextAndJSON(t *testing.T) {
	cfg := testConfig()
	cfg.Data.Retention = config.RetentionConfig{
		MaxAgeDays:  intPtr(0),
		Enforcement: config.RetentionManualOnly,
	}
	path := saveTestConfig(t, cfg)
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"config", "retention", "get"}); err != nil {
		t.Fatalf("Execute text: %v", err)
	}
	want := "Data retention:\n  Max age days: 0\n  Enforcement: manual_only\n"
	if out.String() != want {
		t.Fatalf("retention text = %q, want %q", out.String(), want)
	}

	cmd, out = newTestCommand(path)
	if err := root.Execute(cmd, []string{"config", "retention", "get", "--json"}); err != nil {
		t.Fatalf("Execute JSON: %v", err)
	}
	var got view.ConfigRetention
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.MaxAgeDays != 0 || got.Enforcement != "manual_only" {
		t.Fatalf("retention JSON = %#v, want keep forever manual_only", got)
	}
}

func TestConfigRetentionGetReadsSavedMutation(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	cmd, _ := newTestCommand(path)

	if err := root.Execute(cmd, []string{"config", "retention", "set", "--max-age-days", "12", "--enforcement", "manual_only"}); err != nil {
		t.Fatalf("Execute set: %v", err)
	}
	cmd, out := newTestCommand(path)
	if err := root.Execute(cmd, []string{"config", "retention", "get", "--json"}); err != nil {
		t.Fatalf("Execute get: %v", err)
	}
	var got view.ConfigRetention
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.MaxAgeDays != 12 || got.Enforcement != "manual_only" {
		t.Fatalf("retention JSON = %#v, want saved mutation", got)
	}
}

func TestConfigRetentionSetMutatesAndPreservesUnrelatedConfig(t *testing.T) {
	cfg := testConfig()
	cfg.RepositoryProfiles = []config.RepositoryProfile{{
		Profile: "work",
		Match: config.RepositoryProfileMatch{
			Host:      "github.com",
			Namespace: "open-cli-collective",
		},
	}}
	path := saveTestConfig(t, cfg)
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"config", "retention", "set", "--max-age-days", "30", "--enforcement", "manual_only"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := out.String(); got != "Data retention:\n  Max age days: 30\n  Enforcement: manual_only\n" {
		t.Fatalf("stdout = %q, want updated retention text", got)
	}
	saved, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if saved.Data.Retention.MaxAgeDaysValue() != 30 || saved.Data.Retention.Enforcement != config.RetentionManualOnly {
		t.Fatalf("retention = %#v, want 30/manual_only", saved.Data.Retention)
	}
	if !reflect.DeepEqual(saved.Profiles, config.Normalize(cfg).Profiles) {
		t.Fatalf("profiles = %#v, want preserved", saved.Profiles)
	}
	if !reflect.DeepEqual(saved.RepositoryProfiles, cfg.RepositoryProfiles) {
		t.Fatalf("repository_profiles = %#v, want preserved", saved.RepositoryProfiles)
	}
	if !reflect.DeepEqual(saved.Keyring, cfg.Keyring) {
		t.Fatalf("keyring = %#v, want preserved", saved.Keyring)
	}
}

func TestConfigRetentionSetPartialUpdates(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	cmd, _ := newTestCommand(path)

	if err := root.Execute(cmd, []string{"config", "retention", "set", "--max-age-days", "45"}); err != nil {
		t.Fatalf("Execute max age: %v", err)
	}
	saved, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load after max age: %v", err)
	}
	if saved.Data.Retention.MaxAgeDaysValue() != 45 || saved.Data.Retention.Enforcement != config.RetentionAtWrite {
		t.Fatalf("retention after max age = %#v, want 45/at_write", saved.Data.Retention)
	}

	cmd, _ = newTestCommand(path)
	if err := root.Execute(cmd, []string{"config", "retention", "set", "--enforcement", "manual_only"}); err != nil {
		t.Fatalf("Execute enforcement: %v", err)
	}
	saved, err = config.Load(path)
	if err != nil {
		t.Fatalf("Load after enforcement: %v", err)
	}
	if saved.Data.Retention.MaxAgeDaysValue() != 45 || saved.Data.Retention.Enforcement != config.RetentionManualOnly {
		t.Fatalf("retention after enforcement = %#v, want 45/manual_only", saved.Data.Retention)
	}

	cmd, _ = newTestCommand(path)
	if err := root.Execute(cmd, []string{"config", "retention", "set", "--max-age-days", "0", "--enforcement", "at_write"}); err != nil {
		t.Fatalf("Execute explicit zero: %v", err)
	}
	saved, err = config.Load(path)
	if err != nil {
		t.Fatalf("Load after explicit zero: %v", err)
	}
	if saved.Data.Retention.MaxAgeDaysValue() != 0 || saved.Data.Retention.Enforcement != config.RetentionAtWrite {
		t.Fatalf("retention after explicit zero = %#v, want 0/at_write", saved.Data.Retention)
	}
}

func TestConfigRetentionSetPreservesExplicitZeroOnPartialUpdate(t *testing.T) {
	cfg := testConfig()
	cfg.Data.Retention = config.RetentionConfig{
		MaxAgeDays:  intPtr(0),
		Enforcement: config.RetentionAtWrite,
	}
	path := saveTestConfig(t, cfg)
	cmd, _ := newTestCommand(path)

	if err := root.Execute(cmd, []string{"config", "retention", "set", "--enforcement", "manual_only"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	saved, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if saved.Data.Retention.MaxAgeDaysValue() != 0 || saved.Data.Retention.Enforcement != config.RetentionManualOnly {
		t.Fatalf("retention = %#v, want 0/manual_only", saved.Data.Retention)
	}
}

func TestConfigRetentionSetRejectsInvalidInputs(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "no flags", args: []string{"config", "retention", "set"}, want: "requires --max-age-days or --enforcement"},
		{name: "negative max age", args: []string{"config", "retention", "set", "--max-age-days", "-1"}, want: "--max-age-days must be non-negative"},
		{name: "bad enforcement", args: []string{"config", "retention", "set", "--enforcement", "sometimes"}, want: "--enforcement must be one of at_write, manual_only"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// #nosec G304 -- test path is controlled by t.TempDir via saveTestConfig.
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile before: %v", err)
			}
			cmd, _ := newTestCommand(path)
			err = root.Execute(cmd, tt.args)
			if err == nil {
				t.Fatal("Execute error = nil, want usage error")
			}
			if got := exitcode.FromError(err); got != exitcode.UsageError {
				t.Fatalf("exit code = %d, want %d", got, exitcode.UsageError)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want %q", err, tt.want)
			}
			// #nosec G304 -- test path is controlled by t.TempDir via saveTestConfig.
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile after: %v", err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("config changed after invalid input\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

func TestConfigRetentionResetRestoresDefaultsAndPreservesConfig(t *testing.T) {
	cfg := testConfig()
	cfg.RepositoryProfiles = []config.RepositoryProfile{{
		Profile: "work",
		Match: config.RepositoryProfileMatch{
			Host:      "github.com",
			Namespace: "open-cli-collective",
		},
	}}
	cfg.Data.Retention = config.RetentionConfig{
		MaxAgeDays:  intPtr(0),
		Enforcement: config.RetentionManualOnly,
	}
	path := saveTestConfig(t, cfg)
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"config", "retention", "reset"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := out.String(); got != "Data retention:\n  Max age days: 90\n  Enforcement: at_write\n" {
		t.Fatalf("stdout = %q, want default retention text", got)
	}
	saved, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if saved.Data.Retention.MaxAgeDaysValue() != 90 || saved.Data.Retention.Enforcement != config.RetentionAtWrite {
		t.Fatalf("retention = %#v, want 90/at_write", saved.Data.Retention)
	}
	if !reflect.DeepEqual(saved.Profiles, config.Normalize(cfg).Profiles) {
		t.Fatalf("profiles = %#v, want preserved", saved.Profiles)
	}
	if !reflect.DeepEqual(saved.RepositoryProfiles, cfg.RepositoryProfiles) {
		t.Fatalf("repository_profiles = %#v, want preserved", saved.RepositoryProfiles)
	}
	if !reflect.DeepEqual(saved.Keyring, cfg.Keyring) {
		t.Fatalf("keyring = %#v, want preserved", saved.Keyring)
	}
}

func TestConfigLLMModelsListAndResolve(t *testing.T) {
	path := saveTestConfig(t, testConfig())

	cmd, out := newTestCommand(path)
	if err := root.Execute(cmd, []string{"--profile", "home", "config", "llm", "models", "list"}); err != nil {
		t.Fatalf("Execute list: %v", err)
	}
	if !strings.Contains(out.String(), "small: <unset> (unset)") ||
		!strings.Contains(out.String(), "medium: claude-sonnet-4-6 (built_in)") ||
		!strings.Contains(out.String(), "large: claude-opus-4-8 (built_in)") {
		t.Fatalf("list stdout = %q, want effective Claude CLI defaults", out.String())
	}

	cmd, out = newTestCommand(path)
	if err := root.Execute(cmd, []string{"--profile", "home", "config", "llm", "models", "list", "--json"}); err != nil {
		t.Fatalf("Execute list json: %v", err)
	}
	var listed modelMapResultView
	if err := json.Unmarshal(out.Bytes(), &listed); err != nil {
		t.Fatalf("Unmarshal list JSON: %v\n%s", err, out.String())
	}
	if listed.ActiveProfile != "home" || len(listed.Models) != 3 || listed.Models[1].Model != "claude-sonnet-4-6" || listed.Models[1].Source != "built_in" {
		t.Fatalf("list JSON = %#v, want home built-in medium", listed)
	}

	cmd, out = newTestCommand(path)
	if err := root.Execute(cmd, []string{"--profile", "home", "config", "llm", "models", "resolve", "medium", "--json"}); err != nil {
		t.Fatalf("Execute resolve json: %v", err)
	}
	var resolved modelResolveResult
	if err := json.Unmarshal(out.Bytes(), &resolved); err != nil {
		t.Fatalf("Unmarshal resolve JSON: %v\n%s", err, out.String())
	}
	if resolved.Model != "claude-sonnet-4-6" || resolved.Source != "built_in" || resolved.Tier != "medium" {
		t.Fatalf("resolve JSON = %#v, want built-in medium claude-sonnet-4-6", resolved)
	}
}

func TestConfigLLMModelsSetUnsetAndReset(t *testing.T) {
	path := saveTestConfig(t, testConfig())

	cmd, out := newTestCommand(path)
	if err := root.Execute(cmd, []string{"--profile", "home", "config", "llm", "models", "set", "medium", "claude-custom"}); err != nil {
		t.Fatalf("Execute set: %v", err)
	}
	if !strings.Contains(out.String(), "Set medium: claude-custom") {
		t.Fatalf("set stdout = %q", out.String())
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load after set: %v", err)
	}
	if got := cfg.Profiles["home"].LLM.ModelMap["medium"]; got != "claude-custom" {
		t.Fatalf("model_map.medium = %q, want claude-custom", got)
	}

	cmd, out = newTestCommand(path)
	if err := root.Execute(cmd, []string{"--profile", "home", "config", "llm", "models", "unset", "medium"}); err != nil {
		t.Fatalf("Execute unset: %v", err)
	}
	if !strings.Contains(out.String(), "Unset medium") {
		t.Fatalf("unset stdout = %q", out.String())
	}
	cfg, err = config.Load(path)
	if err != nil {
		t.Fatalf("Load after unset: %v", err)
	}
	if cfg.Profiles["home"].LLM.ModelMap != nil {
		t.Fatalf("model_map after unset = %#v, want nil", cfg.Profiles["home"].LLM.ModelMap)
	}

	cmd, _ = newTestCommand(path)
	if err := root.Execute(cmd, []string{"--profile", "home", "config", "llm", "models", "set", "large", "claude-large"}); err != nil {
		t.Fatalf("Execute second set: %v", err)
	}
	cmd, out = newTestCommand(path)
	if err := root.Execute(cmd, []string{"--profile", "home", "config", "llm", "models", "reset", "--provider", "anthropic"}); err != nil {
		t.Fatalf("Execute reset: %v", err)
	}
	if !strings.Contains(out.String(), "Reset model map for profile home") {
		t.Fatalf("reset stdout = %q", out.String())
	}
	cfg, err = config.Load(path)
	if err != nil {
		t.Fatalf("Load after reset: %v", err)
	}
	if cfg.Profiles["home"].LLM.ModelMap != nil {
		t.Fatalf("model_map after reset = %#v, want nil", cfg.Profiles["home"].LLM.ModelMap)
	}
}

func TestConfigLLMModelsMutatesSelectedProfileOnly(t *testing.T) {
	cfg := testConfig()
	home := cfg.Profiles["home"]
	home.LLM.ModelMap = config.ModelMap{"medium": "home-model"}
	cfg.Profiles["home"] = home
	path := saveTestConfig(t, cfg)

	cmd, _ := newTestCommand(path)
	if err := root.Execute(cmd, []string{"--profile", "work", "config", "llm", "models", "set", "medium", "work-model"}); err != nil {
		t.Fatalf("Execute work set: %v", err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load after work set: %v", err)
	}
	if got := loaded.Profiles["work"].LLM.ModelMap["medium"]; got != "work-model" {
		t.Fatalf("work model_map.medium = %q, want work-model", got)
	}
	if got := loaded.Profiles["home"].LLM.ModelMap["medium"]; got != "home-model" {
		t.Fatalf("home model_map.medium = %q, want unchanged home-model", got)
	}

	cmd, _ = newTestCommand(path)
	if err := root.Execute(cmd, []string{"--profile", "work", "config", "llm", "models", "unset", "medium"}); err != nil {
		t.Fatalf("Execute work unset: %v", err)
	}
	loaded, err = config.Load(path)
	if err != nil {
		t.Fatalf("Load after work unset: %v", err)
	}
	if loaded.Profiles["work"].LLM.ModelMap != nil {
		t.Fatalf("work model_map after unset = %#v, want nil", loaded.Profiles["work"].LLM.ModelMap)
	}
	if got := loaded.Profiles["home"].LLM.ModelMap["medium"]; got != "home-model" {
		t.Fatalf("home model_map.medium = %q, want unchanged home-model", got)
	}

	work := loaded.Profiles["work"]
	work.LLM.ModelMap = config.ModelMap{"large": "work-large"}
	loaded.Profiles["work"] = work
	if err := config.Save(path, loaded); err != nil {
		t.Fatalf("Save before work reset: %v", err)
	}
	cmd, _ = newTestCommand(path)
	if err := root.Execute(cmd, []string{"--profile", "work", "config", "llm", "models", "reset", "--provider", "anthropic"}); err != nil {
		t.Fatalf("Execute work reset: %v", err)
	}
	loaded, err = config.Load(path)
	if err != nil {
		t.Fatalf("Load after work reset: %v", err)
	}
	if loaded.Profiles["work"].LLM.ModelMap != nil {
		t.Fatalf("work model_map after reset = %#v, want nil", loaded.Profiles["work"].LLM.ModelMap)
	}
	if got := loaded.Profiles["home"].LLM.ModelMap["medium"]; got != "home-model" {
		t.Fatalf("home model_map.medium = %q, want unchanged home-model", got)
	}
}

func TestConfigLLMModelsRejectsInvalidInputs(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	tests := []struct {
		name string
		args []string
	}{
		{name: "bad tier", args: []string{"--profile", "home", "config", "llm", "models", "set", "flagship", "gpt"}},
		{name: "blank model", args: []string{"--profile", "home", "config", "llm", "models", "set", "medium", " \t "}},
		{name: "provider guard mismatch", args: []string{"--profile", "home", "config", "llm", "models", "reset", "--provider", "openai"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, _ := newTestCommand(path)
			err := root.Execute(cmd, tt.args)
			if err == nil {
				t.Fatal("Execute error = nil, want usage error")
			}
			if got := exitcode.FromError(err); got != exitcode.UsageError {
				t.Fatalf("exit code = %d, want usage", got)
			}
		})
	}
}

func TestConfigLLMModelsResolveReportsUnmappedTier(t *testing.T) {
	cfg := testConfig()
	profile := cfg.Profiles["work"]
	profile.LLM.ModelMap = nil
	cfg.Profiles["work"] = profile
	path := saveTestConfig(t, cfg)
	cmd, _ := newTestCommand(path)

	err := root.Execute(cmd, []string{"--profile", "work", "config", "llm", "models", "resolve", "medium"})
	if err == nil || !strings.Contains(err.Error(), `model_tier "medium" is not mapped`) {
		t.Fatalf("Execute error = %v, want unmapped tier", err)
	}
}

func TestRootJSONFlagStillDeferred(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	cmd, _ := newTestCommand(path)

	err := root.Execute(cmd, []string{"--json", "config", "show"})
	if err == nil {
		t.Fatal("Execute root --json error = nil, want usage error")
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.UsageError)
	}
}

func TestConfigShowMissingConfigExitCode(t *testing.T) {
	cmd, _ := newTestCommand(filepath.Join(t.TempDir(), "missing.yml"))

	err := root.Execute(cmd, []string{"config", "show"})
	if !errors.Is(err, config.ErrNotConfigured) {
		t.Fatalf("Execute error = %v, want ErrNotConfigured", err)
	}
	if got := exitcode.FromError(err); got != exitcode.AuthConfigError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.AuthConfigError)
	}
}

func TestConfigShowMissingProfileExitCode(t *testing.T) {
	path := saveTestConfig(t, testConfig())
	cmd, _ := newTestCommand(path)

	err := root.Execute(cmd, []string{"config", "show", "--profile", "missing"})
	if !errors.Is(err, config.ErrProfileNotFound) {
		t.Fatalf("Execute error = %v, want ErrProfileNotFound", err)
	}
	if got := exitcode.FromError(err); got != exitcode.AuthConfigError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.AuthConfigError)
	}
}

func TestConfigShowInvalidEnumExitCode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	writeRawConfig(t, path, `secrets:
  stores:
    test-memory:
      backend:
        kind: memory
profiles:
  home:
    git:
      host: github.com
      auth_mode: pat
      credential:
        store: test-memory
        name: codereview/home
    llm:
      provider: anthropic
      auth: subscription
      adapter: nope
`)
	cmd, _ := newTestCommand(path)

	err := root.Execute(cmd, []string{"config", "show"})
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("Execute error = %v, want ErrInvalid", err)
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.UsageError)
	}
}

func TestConfigShowReservedAuthModeExitCode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	writeRawConfig(t, path, `secrets:
  stores:
    test-memory:
      backend:
        kind: memory
llm_runtimes:
  claude-cli:
    provider: anthropic
    auth: subscription
    adapter: claude_cli
profiles:
  home:
    git:
      host: github.com
      auth_mode: oauth_device
      credential:
        store: test-memory
        name: codereview/home
    reviewer:
      kind: git_identity
    llm_runtime: claude-cli
`)
	cmd, _ := newTestCommand(path)

	err := root.Execute(cmd, []string{"config", "show"})
	if !errors.Is(err, config.ErrUnsupported) {
		t.Fatalf("Execute error = %v, want ErrUnsupported", err)
	}
	if got := exitcode.FromError(err); got != exitcode.AuthConfigError {
		t.Fatalf("exit code = %d, want %d", got, exitcode.AuthConfigError)
	}
}

func TestConfigClearDefaultClearsActiveProfileOnlyAndPreservesData(t *testing.T) {
	path := saveTestConfig(t, fileBackendConfig(t))
	dataFile := writeDataSentinel(t)
	seedFileBackend(t, "home", map[string]string{credentials.GitTokenKey: "home-token"})
	seedFileBackend(t, "work", map[string]string{credentials.GitTokenKey: "work-token"})
	seedFileBackend(t, "work-llm", map[string]string{credentials.AnthropicAPIKeyKey: "llm-token"})
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"--profile", "home", "config", "clear", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.ConfigClear
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if len(got.Cleared) != 1 || got.Cleared[0].Ref != "codereview/home" {
		t.Fatalf("cleared = %#v, want active home only", got.Cleared)
	}
	assertFileBackendMissing(t, "home", credentials.GitTokenKey)
	assertFileBackendPresent(t, "work", credentials.GitTokenKey)
	assertFileBackendKeys(t, "work-llm", []string{credentials.AnthropicAPIKeyKey})
	// #nosec G304,G703 -- test path is controlled by t.TempDir via XDG_DATA_HOME.
	if got, err := os.ReadFile(dataFile); err != nil || string(got) != "keep" {
		t.Fatalf("data sentinel = (%q,%v), want kept", got, err)
	}
}

func TestConfigClearDryRunReportsActiveProfileAndPreservesState(t *testing.T) {
	path := saveTestConfig(t, fileBackendConfig(t))
	// #nosec G304 -- test path is controlled by t.TempDir via saveTestConfig.
	beforeConfig, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile config before dry-run: %v", err)
	}
	dataFile := writeDataSentinel(t)
	seedFileBackend(t, "home", map[string]string{credentials.GitTokenKey: "home-token"})
	seedFileBackend(t, "work", map[string]string{credentials.GitTokenKey: "work-token"})
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"--profile", "home", "config", "clear", "--dry-run", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.ConfigClear
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if !got.DryRun {
		t.Fatalf("dry_run = false, want true")
	}
	if len(got.Cleared) != 1 || got.Cleared[0].Ref != "codereview/home" {
		t.Fatalf("cleared = %#v, want active home only", got.Cleared)
	}
	if !reflect.DeepEqual(got.Cleared[0].Keys, []string{credentials.GitTokenKey}) {
		t.Fatalf("dry-run keys = %#v, want git token key", got.Cleared[0].Keys)
	}
	assertFileBackendPresent(t, "home", credentials.GitTokenKey)
	assertFileBackendPresent(t, "work", credentials.GitTokenKey)
	// #nosec G304 -- test path is controlled by t.TempDir via saveTestConfig.
	afterConfig, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile config after dry-run: %v", err)
	}
	if !bytes.Equal(afterConfig, beforeConfig) {
		t.Fatalf("config changed during dry-run\nbefore:\n%s\nafter:\n%s", beforeConfig, afterConfig)
	}
	// #nosec G304,G703 -- test path is controlled by t.TempDir via XDG_DATA_HOME.
	if got, err := os.ReadFile(dataFile); err != nil || string(got) != "keep" {
		t.Fatalf("data sentinel = (%q,%v), want kept", got, err)
	}
}

func TestConfigClearGitHubAppCredentialMatrix(t *testing.T) {
	cfg := fileBackendConfig(t)
	work := cfg.Profiles["work"]
	work.Git.AuthMode = config.GitAuthModeGitHubApp
	work.Git.GitHubApp = &config.GitHubAppConfig{AppID: "12345"}
	work.Git.CredentialRef = "codereview/work-app"
	work.Git.Credential.Name = "codereview/work-app"
	cfg.Profiles["work"] = work
	path := saveTestConfig(t, cfg)
	appKeys := []string{
		credentials.GitHubAppPrivateKeyKey,
	}
	seedFileBackend(t, "work-app", map[string]string{
		credentials.GitHubAppPrivateKeyKey: "private-key",
	})

	dryRunCmd, dryRunOut := newTestCommand(path)
	if err := root.Execute(dryRunCmd, []string{"--profile", "work", "config", "clear", "--dry-run", "--json"}); err != nil {
		t.Fatalf("Execute dry-run: %v", err)
	}
	var dryRun view.ConfigClear
	if err := json.Unmarshal(dryRunOut.Bytes(), &dryRun); err != nil {
		t.Fatalf("Unmarshal dry-run JSON: %v\n%s", err, dryRunOut.String())
	}
	foundDryRun := false
	for _, cleared := range dryRun.Cleared {
		if cleared.Ref == "codereview/work-app" && reflect.DeepEqual(cleared.Keys, appKeys) {
			foundDryRun = true
			break
		}
	}
	if !foundDryRun {
		t.Fatalf("dry-run cleared = %#v, want github app keys for codereview/work-app", dryRun.Cleared)
	}
	assertFileBackendKeys(t, "work-app", appKeys)

	clearCmd, clearOut := newTestCommand(path)
	if err := root.Execute(clearCmd, []string{"--profile", "work", "config", "clear", "--json"}); err != nil {
		t.Fatalf("Execute clear: %v", err)
	}
	var cleared view.ConfigClear
	if err := json.Unmarshal(clearOut.Bytes(), &cleared); err != nil {
		t.Fatalf("Unmarshal clear JSON: %v\n%s", err, clearOut.String())
	}
	foundClear := false
	for _, entry := range cleared.Cleared {
		if entry.Ref == "codereview/work-app" && reflect.DeepEqual(entry.Keys, appKeys) {
			foundClear = true
			break
		}
	}
	if !foundClear {
		t.Fatalf("cleared = %#v, want github app keys for codereview/work-app", cleared.Cleared)
	}
	for _, key := range appKeys {
		assertFileBackendMissing(t, "work-app", key)
	}
}

func TestConfigClearDryRunTextReportsDryRun(t *testing.T) {
	path := saveTestConfig(t, fileBackendConfig(t))
	seedFileBackend(t, "home", map[string]string{credentials.GitTokenKey: "home-token"})
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"--profile", "home", "config", "clear", "--dry-run"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Dry run: true",
		"Credential targets:",
		"codereview/home: 1 key(s)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("text output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "home-token") {
		t.Fatalf("dry-run text leaked credential value:\n%s", got)
	}
	assertFileBackendPresent(t, "home", credentials.GitTokenKey)
}

func TestConfigClearAllDryRunTextReportsPredictedReset(t *testing.T) {
	cfg := fileBackendConfig(t)
	path := saveTestConfig(t, cfg)
	cacheFile := writeCacheSentinel(t)
	seedFileBackend(t, "home", map[string]string{credentials.GitTokenKey: "home-token"})
	seedFileBackend(t, "work", map[string]string{credentials.GitTokenKey: "work-token"})
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"--profile", "work", "config", "clear", "--all", "--dry-run"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Dry run: true",
		"Credential targets:",
		"Config profile removed: work",
		"Cache status: would_remove",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("text output missing %q:\n%s", want, got)
		}
	}
	assertFileBackendPresent(t, "work", credentials.GitTokenKey)
	if _, err := os.Stat(cacheFile); err != nil {
		t.Fatalf("cache sentinel stat err = %v, want kept", err)
	}
}

func TestConfigClearAllClearsOnlySelectedProfileAndRemovesCache(t *testing.T) {
	cfg := fileBackendConfig(t)
	alpha := cfg.Profiles["home"]
	alpha.Git.CredentialRef = "codereview/alpha"
	alpha.Git.Credential.Name = "codereview/alpha"
	cfg.Profiles["alpha"] = alpha
	beta := cfg.Profiles["home"]
	beta.Git.CredentialRef = "codereview/beta"
	beta.Git.Credential.Name = "codereview/beta"
	cfg.Profiles["beta"] = beta
	path := saveTestConfig(t, cfg)
	cacheFile := writeCacheSentinel(t)
	dataFile := writeDataSentinel(t)
	ledgerFile := writeLedgerSentinel(t)
	seedFileBackend(t, "alpha", map[string]string{credentials.GitTokenKey: "alpha-token"})
	seedFileBackend(t, "beta", map[string]string{credentials.GitTokenKey: "beta-token"})
	seedFileBackend(t, "home", map[string]string{credentials.GitTokenKey: "home-token"})
	seedFileBackend(t, "work", map[string]string{credentials.GitTokenKey: "work-token"})
	seedFileBackend(t, "work-reviewer", map[string]string{credentials.GitTokenKey: "reviewer-token"})
	seedFileBackend(t, "work-llm", map[string]string{credentials.AnthropicAPIKeyKey: "llm-token"})
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"--profile", "home", "config", "clear", "--all", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.ConfigClear
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.Backend != "file" || got.BackendSource != "credential_store" {
		t.Fatalf("backend = (%q,%q), want (file,credential_store)", got.Backend, got.BackendSource)
	}
	if len(got.Cleared) != 1 || got.Cleared[0].Ref != "codereview/home" {
		t.Fatalf("cleared = %#v, want default home ref only", got.Cleared)
	}
	if got.ConfigProfileRemoved != "home" {
		t.Fatalf("config clear profile = %q, want removed home", got.ConfigProfileRemoved)
	}
	if got.ConfigPathRemoved != "" {
		t.Fatalf("config_path_removed = %q, want empty because work remains", got.ConfigPathRemoved)
	}
	if got.Cache == nil || got.Cache.Path == "" || got.Cache.Status != "removed" {
		t.Fatalf("cache = %#v, want removed cache path", got.Cache)
	}
	assertFileBackendMissing(t, "home", credentials.GitTokenKey)
	assertFileBackendPresent(t, "alpha", credentials.GitTokenKey)
	assertFileBackendPresent(t, "beta", credentials.GitTokenKey)
	assertFileBackendPresent(t, "work", credentials.GitTokenKey)
	assertFileBackendPresent(t, "work-reviewer", credentials.GitTokenKey)
	assertFileBackendKeys(t, "work-llm", []string{credentials.AnthropicAPIKeyKey})
	if _, err := os.Stat(cacheFile); !os.IsNotExist(err) {
		t.Fatalf("cache sentinel stat err = %v, want removed", err)
	}
	// #nosec G304 -- test path is controlled by t.TempDir via XDG_DATA_HOME.
	if got, err := os.ReadFile(dataFile); err != nil || string(got) != "keep" {
		t.Fatalf("data sentinel = (%q,%v), want kept", got, err)
	}
	// #nosec G304 -- test path is controlled by t.TempDir via XDG_DATA_HOME.
	if got, err := os.ReadFile(ledgerFile); err != nil || string(got) != "ledger" {
		t.Fatalf("ledger sentinel = (%q,%v), want kept", got, err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load remaining config: %v", err)
	}
	if len(cfg.Profiles) != 3 {
		t.Fatalf("profiles len = %d, want alpha/beta/work", len(cfg.Profiles))
	}
	if _, ok := cfg.Profiles["home"]; ok {
		t.Fatalf("home profile still present after --all: %#v", cfg.Profiles)
	}
	if _, ok := cfg.Profiles["alpha"]; !ok {
		t.Fatalf("alpha profile missing after clearing home: %#v", cfg.Profiles)
	}
	if _, ok := cfg.Profiles["beta"]; !ok {
		t.Fatalf("beta profile missing after clearing home: %#v", cfg.Profiles)
	}
	if _, ok := cfg.Profiles["work"]; !ok {
		t.Fatalf("work profile missing after clearing home: %#v", cfg.Profiles)
	}
}

func TestConfigClearAllDryRunReportsProfileCacheAndPreservesState(t *testing.T) {
	cfg := fileBackendConfig(t)
	cfg.RepositoryProfiles = []config.RepositoryProfile{
		{
			Profile: "work",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
				Repos:     []string{"bar"},
			},
		},
		{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "open-cli-collective",
			},
		},
	}
	path := saveTestConfig(t, cfg)
	// #nosec G304 -- test path is controlled by t.TempDir via saveTestConfig.
	beforeConfig, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile config before dry-run: %v", err)
	}
	cacheFile := writeCacheSentinel(t)
	dataFile := writeDataSentinel(t)
	ledgerFile := writeLedgerSentinel(t)
	seedFileBackend(t, "home", map[string]string{credentials.GitTokenKey: "home-token"})
	seedFileBackend(t, "work", map[string]string{credentials.GitTokenKey: "work-token"})
	seedFileBackend(t, "work-reviewer", map[string]string{credentials.GitTokenKey: "reviewer-token"})
	seedFileBackend(t, "work-llm", map[string]string{credentials.AnthropicAPIKeyKey: "llm-token"})
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"--profile", "work", "config", "clear", "--all", "--dry-run", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.ConfigClear
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if !got.DryRun {
		t.Fatalf("dry_run = false, want true")
	}
	if got.ConfigProfileRemoved != "work" || got.ConfigPathRemoved != "" {
		t.Fatalf("config dry-run fields = profile:%q path:%q, want work removal preview with retained path", got.ConfigProfileRemoved, got.ConfigPathRemoved)
	}
	if got.Cache == nil || got.Cache.Status != "would_remove" {
		t.Fatalf("cache = %#v, want would_remove", got.Cache)
	}
	wantKeys := map[string][]string{
		"codereview/work":          {credentials.GitTokenKey},
		"codereview/work-llm":      {credentials.AnthropicAPIKeyKey},
		"codereview/work-reviewer": {credentials.GitTokenKey},
	}
	if len(got.Cleared) != len(wantKeys) {
		t.Fatalf("cleared = %#v, want %d refs", got.Cleared, len(wantKeys))
	}
	for _, cleared := range got.Cleared {
		want, ok := wantKeys[cleared.Ref]
		if !ok {
			t.Fatalf("unexpected cleared ref %q in %#v", cleared.Ref, got.Cleared)
		}
		if !reflect.DeepEqual(cleared.Keys, want) {
			t.Fatalf("keys for %s = %#v, want %#v", cleared.Ref, cleared.Keys, wantKeys[cleared.Ref])
		}
		delete(wantKeys, cleared.Ref)
	}
	if len(wantKeys) != 0 {
		t.Fatalf("missing cleared refs: %#v", wantKeys)
	}
	assertFileBackendPresent(t, "home", credentials.GitTokenKey)
	assertFileBackendPresent(t, "work", credentials.GitTokenKey)
	assertFileBackendPresent(t, "work-reviewer", credentials.GitTokenKey)
	assertFileBackendKeys(t, "work-llm", []string{credentials.AnthropicAPIKeyKey})
	// #nosec G304 -- test path is controlled by t.TempDir via saveTestConfig.
	afterConfig, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile config after dry-run: %v", err)
	}
	if !bytes.Equal(afterConfig, beforeConfig) {
		t.Fatalf("config changed during dry-run\nbefore:\n%s\nafter:\n%s", beforeConfig, afterConfig)
	}
	if _, err := os.Stat(cacheFile); err != nil {
		t.Fatalf("cache sentinel stat err = %v, want kept", err)
	}
	// #nosec G304 -- test path is controlled by t.TempDir via XDG_DATA_HOME.
	if got, err := os.ReadFile(dataFile); err != nil || string(got) != "keep" {
		t.Fatalf("data sentinel = (%q,%v), want kept", got, err)
	}
	// #nosec G304 -- test path is controlled by t.TempDir via XDG_DATA_HOME.
	if got, err := os.ReadFile(ledgerFile); err != nil || string(got) != "ledger" {
		t.Fatalf("ledger sentinel = (%q,%v), want kept", got, err)
	}
}

func TestConfigClearAllDryRunSingleProfileReportsConfigPathRemoval(t *testing.T) {
	cfg := fileBackendConfig(t)
	cfg.Profiles = map[string]config.Profile{"home": cfg.Profiles["home"]}
	path := saveTestConfig(t, cfg)
	seedFileBackend(t, "home", map[string]string{credentials.GitTokenKey: "home-token"})
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"--profile", "home", "config", "clear", "--all", "--dry-run", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.ConfigClear
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if !got.DryRun || got.ConfigProfileRemoved != "home" || got.ConfigPathRemoved != path {
		t.Fatalf("config dry-run fields = dry:%t profile:%q path:%q, want single-profile removal preview", got.DryRun, got.ConfigProfileRemoved, got.ConfigPathRemoved)
	}
	assertFileBackendPresent(t, "home", credentials.GitTokenKey)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config path stat err = %v, want kept", err)
	}
}

func TestConfigClearAllDryRunReportsMissingCache(t *testing.T) {
	path := saveTestConfig(t, fileBackendConfig(t))
	seedFileBackend(t, "home", map[string]string{credentials.GitTokenKey: "home-token"})
	missingCache := filepath.Join(t.TempDir(), "missing-cache")
	oldResolve := resolveCacheRoot
	resolveCacheRoot = func() (string, error) {
		return missingCache, nil
	}
	t.Cleanup(func() { resolveCacheRoot = oldResolve })
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"--profile", "home", "config", "clear", "--all", "--dry-run", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.ConfigClear
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.Cache == nil || got.Cache.Status != "missing" {
		t.Fatalf("cache = %#v, want missing dry-run status", got.Cache)
	}
	assertFileBackendPresent(t, "home", credentials.GitTokenKey)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config path stat err = %v, want kept", err)
	}
}

func TestConfigClearProfileAllClearsOnlySelectedProfile(t *testing.T) {
	cfg := fileBackendConfig(t)
	cfg.RepositoryProfiles = []config.RepositoryProfile{
		{
			Profile: "work",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
				Repos:     []string{"bar"},
			},
		},
		{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "open-cli-collective",
			},
		},
	}
	path := saveTestConfig(t, cfg)
	cacheFile := writeCacheSentinel(t)
	dataFile := writeDataSentinel(t)
	ledgerFile := writeLedgerSentinel(t)
	seedFileBackend(t, "home", map[string]string{credentials.GitTokenKey: "home-token"})
	seedFileBackend(t, "work", map[string]string{credentials.GitTokenKey: "work-token"})
	seedFileBackend(t, "work-reviewer", map[string]string{credentials.GitTokenKey: "reviewer-token"})
	seedFileBackend(t, "work-llm", map[string]string{credentials.AnthropicAPIKeyKey: "llm-token"})
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"--profile", "work", "config", "clear", "--all", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.ConfigClear
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.ConfigProfileRemoved != "work" {
		t.Fatalf("config clear profile = %q, want removed work", got.ConfigProfileRemoved)
	}
	if got.Cache == nil || got.Cache.Status != "removed" {
		t.Fatalf("cache = %#v, want removed", got.Cache)
	}
	if len(got.Cleared) != 3 {
		t.Fatalf("cleared = %#v, want work git/reviewer/llm refs only", got.Cleared)
	}
	assertFileBackendPresent(t, "home", credentials.GitTokenKey)
	assertFileBackendMissing(t, "work", credentials.GitTokenKey)
	assertFileBackendMissing(t, "work-reviewer", credentials.GitTokenKey)
	assertFileBackendMissing(t, "work-llm", credentials.AnthropicAPIKeyKey)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load remaining config: %v", err)
	}
	if len(cfg.Profiles) != 1 {
		t.Fatalf("remaining config = %#v, want home only", cfg)
	}
	if len(cfg.RepositoryProfiles) != 1 || cfg.RepositoryProfiles[0].Profile != "home" {
		t.Fatalf("repository_profiles = %#v, want only home route after removing work", cfg.RepositoryProfiles)
	}
	if _, err := os.Stat(cacheFile); !os.IsNotExist(err) {
		t.Fatalf("cache sentinel stat err = %v, want removed", err)
	}
	// #nosec G304 -- test path is controlled by t.TempDir via XDG_DATA_HOME.
	if got, err := os.ReadFile(dataFile); err != nil || string(got) != "keep" {
		t.Fatalf("data sentinel = (%q,%v), want kept", got, err)
	}
	// #nosec G304 -- test path is controlled by t.TempDir via XDG_DATA_HOME.
	if got, err := os.ReadFile(ledgerFile); err != nil || string(got) != "ledger" {
		t.Fatalf("ledger sentinel = (%q,%v), want kept", got, err)
	}
}

func TestConfigClearProfileAllPrunesSelectedProfileRoutes(t *testing.T) {
	cfg := fileBackendConfig(t)
	alpha := cfg.Profiles["home"]
	alpha.Git.CredentialRef = "codereview/alpha"
	alpha.Git.Credential.Name = "codereview/alpha"
	cfg.Profiles["alpha"] = alpha
	cfg.RepositoryProfiles = []config.RepositoryProfile{
		{
			Profile: "work",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "rianjs",
				Repos:     []string{"bar"},
			},
		},
		{
			Profile: "home",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "open-cli-collective",
			},
		},
		{
			Profile: "alpha",
			Match: config.RepositoryProfileMatch{
				Host:      "github.com",
				Namespace: "example",
			},
		},
	}
	path := saveTestConfig(t, cfg)
	seedFileBackend(t, "alpha", map[string]string{credentials.GitTokenKey: "alpha-token"})
	seedFileBackend(t, "home", map[string]string{credentials.GitTokenKey: "home-token"})
	seedFileBackend(t, "work", map[string]string{credentials.GitTokenKey: "work-token"})
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"--profile", "work", "config", "clear", "--all", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.ConfigClear
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.ConfigProfileRemoved != "work" {
		t.Fatalf("config clear profile = %q, want removed work", got.ConfigProfileRemoved)
	}
	assertFileBackendMissing(t, "work", credentials.GitTokenKey)
	assertFileBackendPresent(t, "alpha", credentials.GitTokenKey)
	assertFileBackendPresent(t, "home", credentials.GitTokenKey)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load remaining config: %v", err)
	}
	if len(cfg.Profiles) != 2 {
		t.Fatalf("remaining config = %#v, want alpha/home only", cfg)
	}
	if len(cfg.RepositoryProfiles) != 2 {
		t.Fatalf("repository_profiles = %#v, want two routes after pruning work", cfg.RepositoryProfiles)
	}
	for _, route := range cfg.RepositoryProfiles {
		if route.Profile == "work" {
			t.Fatalf("repository_profiles = %#v, want work route pruned", cfg.RepositoryProfiles)
		}
	}
	if _, ok := cfg.Profiles["work"]; ok {
		t.Fatalf("work profile still present after --all: %#v", cfg.Profiles)
	}
}

func TestConfigClearAllSingleProfileRemovesConfigFileAndEmptyParent(t *testing.T) {
	cfg := fileBackendConfig(t)
	cfg.Profiles = map[string]config.Profile{"home": cfg.Profiles["home"]}
	configHome := t.TempDir()
	path := saveTestConfigAt(t, filepath.Join(configHome, statepaths.AppDir, "config.yml"), cfg)
	configDir := filepath.Dir(path)
	cacheFile := writeCacheSentinel(t)
	dataFile := writeDataSentinel(t)
	seedFileBackend(t, "home", map[string]string{credentials.GitTokenKey: "home-token"})
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"--profile", "home", "config", "clear", "--all", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.ConfigClear
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.ConfigProfileRemoved != "home" || got.ConfigPathRemoved != path {
		t.Fatalf("config clear fields = profile:%q path:%q, want removed home config", got.ConfigProfileRemoved, got.ConfigPathRemoved)
	}
	assertFileBackendMissing(t, "home", credentials.GitTokenKey)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("config path stat err = %v, want removed", err)
	}
	if _, err := os.Stat(configDir); !os.IsNotExist(err) {
		t.Fatalf("config dir stat err = %v, want owned config dir removed", err)
	}
	if _, err := os.Stat(configHome); err != nil {
		t.Fatalf("config home stat err = %v, want parent directory preserved", err)
	}
	if _, err := os.Stat(cacheFile); !os.IsNotExist(err) {
		t.Fatalf("cache sentinel stat err = %v, want removed", err)
	}
	// #nosec G304 -- test path is controlled by t.TempDir via XDG_DATA_HOME.
	if got, err := os.ReadFile(dataFile); err != nil || string(got) != "keep" {
		t.Fatalf("data sentinel = (%q,%v), want kept", got, err)
	}
}

func TestConfigClearAllJSONIncludesCacheCleanupFailure(t *testing.T) {
	path := saveTestConfig(t, fileBackendConfig(t))
	cacheFile := writeCacheSentinel(t)
	seedFileBackend(t, "home", map[string]string{credentials.GitTokenKey: "home-token"})
	cmd, out := newTestCommand(path)
	oldRemove := removeCacheRoot
	removeCacheRoot = func(string) error {
		return fmt.Errorf("permission denied")
	}
	t.Cleanup(func() { removeCacheRoot = oldRemove })

	err := root.Execute(cmd, []string{"--profile", "home", "config", "clear", "--all", "--json"})
	if err == nil {
		t.Fatal("Execute error = nil, want cache cleanup failure")
	}
	if !strings.Contains(err.Error(), "cache cleanup failed") {
		t.Fatalf("error = %v, want cache cleanup context", err)
	}
	var got view.ConfigClear
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.ConfigProfileRemoved != "home" || got.Cache == nil {
		t.Fatalf("config clear JSON = %#v, want removed profile and cache status", got)
	}
	if got.Cache.Status != "error" || !strings.Contains(got.Cache.Error, "permission denied") {
		t.Fatalf("cache = %#v, want structured error status", got.Cache)
	}
	if _, err := os.Stat(cacheFile); err != nil {
		t.Fatalf("cache sentinel stat err = %v, want cache to remain after failed removal", err)
	}
	assertFileBackendMissing(t, "home", credentials.GitTokenKey)
}

func TestConfigClearAllJSONKeepsStdoutCleanAndReportsProgressErrorOnStderr(t *testing.T) {
	path := saveTestConfig(t, fileBackendConfig(t))
	_ = writeCacheSentinel(t)
	seedFileBackend(t, "home", map[string]string{credentials.GitTokenKey: "home-token"})
	cmd, out, errOut := newTestCommandWithStderr(path, false)
	oldRemove := removeCacheRoot
	removeCacheRoot = func(string) error {
		return fmt.Errorf("permission denied")
	}
	t.Cleanup(func() { removeCacheRoot = oldRemove })

	err := root.Execute(cmd, []string{"--profile", "home", "config", "clear", "--all", "--json"})
	if err == nil {
		t.Fatal("Execute error = nil, want cache cleanup failure")
	}
	var got view.ConfigClear
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\nstdout=%s\nstderr=%s", err, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), `command="config.clear" op="remove_cache" target="home"`) {
		t.Fatalf("stderr = %q, want remove_cache progress", errOut.String())
	}
	if !strings.Contains(errOut.String(), `event=error`) {
		t.Fatalf("stderr = %q, want error event", errOut.String())
	}
}

func TestConfigClearAllTextIncludesPartialResultOnCacheFailure(t *testing.T) {
	path := saveTestConfig(t, fileBackendConfig(t))
	cacheFile := writeCacheSentinel(t)
	seedFileBackend(t, "home", map[string]string{credentials.GitTokenKey: "home-token"})
	cmd, out := newTestCommand(path)
	oldRemove := removeCacheRoot
	removeCacheRoot = func(string) error {
		return fmt.Errorf("permission denied")
	}
	t.Cleanup(func() { removeCacheRoot = oldRemove })

	err := root.Execute(cmd, []string{"--profile", "home", "config", "clear", "--all"})
	if err == nil {
		t.Fatal("Execute error = nil, want cache cleanup failure")
	}
	got := out.String()
	for _, want := range []string{
		"Cleared credentials:",
		"Config profile removed: home",
		"Cache status: error",
		"Cache error: permission denied",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("text output missing %q:\n%s", want, got)
		}
	}
	if _, err := os.Stat(cacheFile); err != nil {
		t.Fatalf("cache sentinel stat err = %v, want cache to remain after failed removal", err)
	}
	assertFileBackendMissing(t, "home", credentials.GitTokenKey)
}

func TestConfigClearAllJSONIncludesCacheResolutionFailure(t *testing.T) {
	path := saveTestConfig(t, fileBackendConfig(t))
	seedFileBackend(t, "home", map[string]string{credentials.GitTokenKey: "home-token"})
	cmd, out := newTestCommand(path)
	oldResolve := resolveCacheRoot
	resolveCacheRoot = func() (string, error) {
		return "", fmt.Errorf("xdg cache unavailable")
	}
	t.Cleanup(func() { resolveCacheRoot = oldResolve })

	err := root.Execute(cmd, []string{"--profile", "home", "config", "clear", "--all", "--json"})
	if err == nil {
		t.Fatal("Execute error = nil, want cache resolution failure")
	}
	if !strings.Contains(err.Error(), "cache cleanup failed for cache root") {
		t.Fatalf("error = %v, want generic cache root context", err)
	}
	var got view.ConfigClear
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.Cache == nil || got.Cache.Status != "error" || !strings.Contains(got.Cache.Error, "xdg cache unavailable") {
		t.Fatalf("cache = %#v, want structured resolution error", got.Cache)
	}
	assertFileBackendMissing(t, "home", credentials.GitTokenKey)
}

func TestConfigClearAllReportsConfigMutationFailureAfterCredentialDelete(t *testing.T) {
	path := saveTestConfig(t, fileBackendConfig(t))
	seedFileBackend(t, "home", map[string]string{credentials.GitTokenKey: "home-token"})
	cmd, _ := newTestCommand(path)
	oldSave := saveConfigFile
	saveConfigFile = func(string, config.File) error {
		return fmt.Errorf("disk full")
	}
	t.Cleanup(func() { saveConfigFile = oldSave })

	err := root.Execute(cmd, []string{"--profile", "home", "config", "clear", "--all"})
	if err == nil {
		t.Fatal("Execute error = nil, want config mutation failure")
	}
	if !strings.Contains(err.Error(), "credentials already cleared") || !strings.Contains(err.Error(), "codereview/home") {
		t.Fatalf("error = %v, want partial-clear context", err)
	}
	assertFileBackendMissing(t, "home", credentials.GitTokenKey)
	cfg, loadErr := config.Load(path)
	if loadErr != nil {
		t.Fatalf("Load config after failed save: %v", loadErr)
	}
	if _, ok := cfg.Profiles["home"]; !ok {
		t.Fatalf("home profile missing from on-disk config after failed save: %#v", cfg.Profiles)
	}
}

func newTestCommand(path string) (*cobra.Command, *bytes.Buffer) {
	return newTestCommandWithOptions(&root.Options{
		ConfigPath: path,
		Stdin:      strings.NewReader(""),
		Quiet:      true,
	})
}

func newTestCommandWithOptions(opts *root.Options) (*cobra.Command, *bytes.Buffer) {
	var out bytes.Buffer
	var errOut bytes.Buffer
	if opts == nil {
		opts = &root.Options{}
	}
	opts.Stdin = strings.NewReader("")
	opts.Stdout = &out
	opts.Stderr = &errOut
	cmd, opts := root.NewCommandWithOptions(opts)
	Register(cmd, opts)
	return cmd, &out
}

func newTestCommandWithStderr(path string, quiet bool) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd, opts := root.NewCommandWithOptions(&root.Options{
		ConfigPath: path,
		Stdin:      strings.NewReader(""),
		Stdout:     &out,
		Stderr:     &errOut,
		Quiet:      quiet,
	})
	Register(cmd, opts)
	return cmd, &out, &errOut
}

const testFileCredentialStoreID = "test-file"

func fileBackendConfig(t *testing.T) config.File {
	t.Helper()
	statedirtest.Hermetic(t)
	t.Setenv("CODEREVIEW_KEYRING_PASSPHRASE", "test-passphrase")
	cfg := testConfig()
	cfg.Secrets.Stores[testFileCredentialStoreID] = config.SecretsStore{
		DisplayName: "Test File Store",
		Backend:     config.SecretsStoreBackend{Kind: config.SecretsBackendKind(credstore.BackendFile)},
	}
	return withCredentialStore(cfg, testFileCredentialStoreID)
}

func withCredentialStore(cfg config.File, storeID string) config.File {
	for name, profile := range cfg.Profiles {
		if profile.Git.Credential.Name != "" {
			profile.Git.Credential.Store = storeID
		}
		if profile.ReviewerCredentials != nil && profile.ReviewerCredentials.Credential.Name != "" {
			profile.ReviewerCredentials.Credential.Store = storeID
		}
		if profile.LLM.Credential.Name != "" {
			profile.LLM.Credential.Store = storeID
		}
		cfg.Profiles[name] = profile
	}
	return cfg
}

func writeDataSentinel(t *testing.T) string {
	t.Helper()
	dataRoot, err := statepaths.DataRoot()
	if err != nil {
		t.Fatalf("DataRoot: %v", err)
	}
	dataFile := filepath.Join(dataRoot, "runs", "sentinel.txt")
	// #nosec G703 -- test path is controlled by t.TempDir via XDG_DATA_HOME.
	if err := os.MkdirAll(filepath.Dir(dataFile), 0o700); err != nil {
		t.Fatalf("MkdirAll data sentinel: %v", err)
	}
	// #nosec G703 -- test path is controlled by t.TempDir via XDG_DATA_HOME.
	if err := os.WriteFile(dataFile, []byte("keep"), 0o600); err != nil {
		t.Fatalf("WriteFile data sentinel: %v", err)
	}
	return dataFile
}

func writeLedgerSentinel(t *testing.T) string {
	t.Helper()
	dataRoot, err := statepaths.DataRoot()
	if err != nil {
		t.Fatalf("DataRoot: %v", err)
	}
	ledgerFile := filepath.Join(dataRoot, "ledger.db")
	// #nosec G703 -- test path is controlled by t.TempDir via XDG_DATA_HOME.
	if err := os.MkdirAll(filepath.Dir(ledgerFile), 0o700); err != nil {
		t.Fatalf("MkdirAll ledger sentinel: %v", err)
	}
	// #nosec G703 -- test path is controlled by t.TempDir via XDG_DATA_HOME.
	if err := os.WriteFile(ledgerFile, []byte("ledger"), 0o600); err != nil {
		t.Fatalf("WriteFile ledger sentinel: %v", err)
	}
	return ledgerFile
}

func writeCacheSentinel(t *testing.T) string {
	t.Helper()
	cacheRoot, err := statepaths.CacheRoot()
	if err != nil {
		t.Fatalf("CacheRoot: %v", err)
	}
	cacheFile := filepath.Join(cacheRoot, "http", "sentinel.txt")
	// #nosec G703 -- test path is controlled by t.TempDir via XDG_CACHE_HOME.
	if err := os.MkdirAll(filepath.Dir(cacheFile), 0o700); err != nil {
		t.Fatalf("MkdirAll cache sentinel: %v", err)
	}
	// #nosec G703 -- test path is controlled by t.TempDir via XDG_CACHE_HOME.
	if err := os.WriteFile(cacheFile, []byte("drop"), 0o600); err != nil {
		t.Fatalf("WriteFile cache sentinel: %v", err)
	}
	return cacheFile
}

func seedFileBackend(t *testing.T, profile string, values map[string]string) {
	t.Helper()
	store, err := credstore.Open(credentials.ServiceName, &credstore.Options{
		AllowedKeys: credentials.AllowedKeys(),
		Backend:     credstore.BackendFile,
	})
	if err != nil {
		t.Fatalf("Open file backend: %v", err)
	}
	defer store.Close()
	if _, err := store.SetBundle(profile, values, credstore.WithOverwrite()); err != nil {
		t.Fatalf("SetBundle(%s): %v", profile, err)
	}
}

func assertFileBackendPresent(t *testing.T, profile, key string) {
	t.Helper()
	if !fileBackendExists(t, profile, key) {
		t.Fatalf("file backend %s/%s missing, want present", profile, key)
	}
}

func assertFileBackendMissing(t *testing.T, profile, key string) {
	t.Helper()
	if fileBackendExists(t, profile, key) {
		t.Fatalf("file backend %s/%s present, want missing", profile, key)
	}
}

func assertFileBackendKeys(t *testing.T, profile string, want []string) {
	t.Helper()
	store, err := credstore.Open(credentials.ServiceName, &credstore.Options{
		AllowedKeys: credentials.AllowedKeys(),
		Backend:     credstore.BackendFile,
	})
	if err != nil {
		t.Fatalf("Open file backend: %v", err)
	}
	defer store.Close()
	got, err := store.ListBundle(profile)
	if err != nil {
		t.Fatalf("ListBundle(%s): %v", profile, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListBundle(%s) = %#v, want %#v", profile, got, want)
	}
}

func fileBackendExists(t *testing.T, profile, key string) bool {
	t.Helper()
	store, err := credstore.Open(credentials.ServiceName, &credstore.Options{
		AllowedKeys: credentials.AllowedKeys(),
		Backend:     credstore.BackendFile,
	})
	if err != nil {
		t.Fatalf("Open file backend: %v", err)
	}
	defer store.Close()
	present, err := store.Exists(profile, key)
	if err != nil {
		t.Fatalf("Exists(%s,%s): %v", profile, key, err)
	}
	return present
}

func saveTestConfig(t *testing.T, cfg config.File) string {
	t.Helper()
	return saveTestConfigAt(t, filepath.Join(t.TempDir(), "config.yml"), cfg)
}

func saveTestConfigAt(t *testing.T, path string, cfg config.File) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll config dir: %v", err)
	}
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return path
}

func writeRawConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

type keychainProbeManifest struct {
	KeychainProbe struct {
		SeedConfig struct {
			Content string `yaml:"content"`
		} `yaml:"seed_config"`
		Command    []string          `yaml:"command"`
		Assertions map[string]string `yaml:"assertions"`
	} `yaml:"keychain_probe"`
}

func readKeychainProbeManifest(t *testing.T) keychainProbeManifest {
	t.Helper()
	path := filepath.Join("..", "..", "..", "packaging", "identity.yml")
	// #nosec G304 -- test reads a fixed repo-local manifest path.
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	var manifest keychainProbeManifest
	if err := yaml.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("yaml.Unmarshal(%s): %v", path, err)
	}
	return manifest
}

func configShowAssertionValue(show view.ConfigShow, key string) (string, bool) {
	switch key {
	case ".backend":
		return show.Backend, true
	case ".backend_source":
		return show.BackendSource, true
	case ".credential_ref":
		return show.CredentialRef, true
	default:
		return "", false
	}
}

func writeConfigTestAgentSource(t *testing.T, root, prompt string) {
	t.Helper()
	category := filepath.Join(root, "harness")
	agent := filepath.Join(category, "reviewer")
	if err := os.MkdirAll(agent, 0o700); err != nil {
		t.Fatalf("MkdirAll agent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(category, "index.yaml"), []byte("name: harness\n"), 0o600); err != nil {
		t.Fatalf("WriteFile category index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agent, "index.yaml"), []byte("name: reviewer\n"), 0o600); err != nil {
		t.Fatalf("WriteFile agent index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agent, "prompt.md"), []byte(prompt), 0o600); err != nil {
		t.Fatalf("WriteFile prompt: %v", err)
	}
}

func testConfig() config.File {
	return config.File{
		Secrets: config.SecretsConfig{
			Stores: map[string]config.SecretsStore{
				"test-memory": {
					DisplayName: "Test Memory Store",
					Backend:     config.SecretsStoreBackend{Kind: config.SecretsBackendKind(credstore.BackendMemory)},
				},
			},
		},
		Profiles: map[string]config.Profile{
			"home": {
				Git: config.GitConfig{
					Host:          "github.com",
					AuthMode:      config.GitAuthModePAT,
					Credential:    config.CredentialLocation{Store: "test-memory", Name: "codereview/home"},
					CredentialRef: "codereview/home",
				},
				LLM: config.LLMConfig{
					Provider: config.LLMProviderAnthropic,
					Auth:     config.LLMAuthSubscription,
					Adapter:  config.LLMAdapterClaudeCLI,
				},
				ReviewPolicy: config.ReviewPolicy{MajorEvent: config.ReviewMajorEventComment},
			},
			"work": {
				Git: config.GitConfig{
					Host:          "github.com",
					AuthMode:      config.GitAuthModePAT,
					Credential:    config.CredentialLocation{Store: "test-memory", Name: "codereview/work"},
					CredentialRef: "codereview/work",
				},
				ReviewerCredentials: &config.ReviewerCredentials{
					AuthMode:      config.GitAuthModePAT,
					Credential:    config.CredentialLocation{Store: "test-memory", Name: "codereview/work-reviewer"},
					CredentialRef: "codereview/work-reviewer",
				},
				LLM: config.LLMConfig{
					Provider:      config.LLMProviderAnthropic,
					Auth:          config.LLMAuthAPIKey,
					Adapter:       config.LLMAdapterAnthropicAPI,
					Credential:    config.CredentialLocation{Store: "test-memory", Name: "codereview/work-llm"},
					CredentialRef: "codereview/work-llm",
				},
				ReviewPolicy: config.ReviewPolicy{MajorEvent: config.ReviewMajorEventRequestChanges},
			},
		},
		Data: config.DataConfig{
			Retention: config.RetentionConfig{
				MaxAgeDays:  intPtr(90),
				Enforcement: config.RetentionAtWrite,
			},
		},
	}
}

func intPtr(value int) *int {
	return &value
}

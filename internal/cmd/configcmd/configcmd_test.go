package configcmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/open-cli-collective/cli-common/credstore"
	"github.com/spf13/cobra"

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

	if err := root.Execute(cmd, []string{"config", "show"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.String(), "Profile: home") {
		t.Fatalf("stdout = %q, want home profile", out.String())
	}
	if !strings.Contains(out.String(), "adapter-managed; not stored by cr") {
		t.Fatalf("stdout = %q, want adapter-managed LLM note", out.String())
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
	if !strings.Contains(out.String(), "Credential ref: codereview/work-llm") {
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
	if got.Backend != "memory" || got.BackendSource != "config" {
		t.Fatalf("backend = (%q,%q), want (memory,config)", got.Backend, got.BackendSource)
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

	if err := root.Execute(cmd, []string{"config", "show", "--json"}); err != nil {
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
	second := got.AgentSources[1]
	if second.Status != agents.SourceStatusMissing || second.Present || second.Error == "" {
		t.Fatalf("second source = %#v, want missing non-fatal source", second)
	}
	third := got.AgentSources[2]
	if third.Status != agents.SourceStatusUnreadable || !third.Present || third.Error == "" {
		t.Fatalf("third source = %#v, want unreadable non-fatal source", third)
	}
}

func TestConfigShowReportsUnknownPresenceWhenKeyringCannotBeQueried(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("CODEREVIEW_KEYRING_PASSPHRASE", "")
	cfg := testConfig()
	cfg.Keyring.Backend = "file"
	path := saveTestConfig(t, cfg)
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"config", "show", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.ConfigShow
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.Backend != "file" || got.BackendSource != "config" {
		t.Fatalf("backend = (%q,%q), want (file,config)", got.Backend, got.BackendSource)
	}
	if len(got.CredentialRefs) != 1 || len(got.CredentialRefs[0].Keys) != 1 {
		t.Fatalf("credential refs = %#v, want one key status", got.CredentialRefs)
	}
	key := got.CredentialRefs[0].Keys[0]
	if key.Status != "unknown" || key.Present != nil || key.Error == "" {
		t.Fatalf("key status = %#v, want unknown with error and no present bool", key)
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
	writeRawConfig(t, path, `default_profile: home
profiles:
  home:
    git:
      host: github.com
      auth_mode: pat
      credential_ref: codereview/home
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
	writeRawConfig(t, path, `default_profile: home
profiles:
  home:
    git:
      host: github.com
      auth_mode: oauth_device
      credential_ref: codereview/home
    llm:
      provider: anthropic
      auth: subscription
      adapter: claude_cli
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

	if err := root.Execute(cmd, []string{"config", "clear", "--json"}); err != nil {
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
	beforeConfig, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile config before dry-run: %v", err)
	}
	dataFile := writeDataSentinel(t)
	seedFileBackend(t, "home", map[string]string{credentials.GitTokenKey: "home-token"})
	seedFileBackend(t, "work", map[string]string{credentials.GitTokenKey: "work-token"})
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"config", "clear", "--dry-run", "--json"}); err != nil {
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

func TestConfigClearDryRunTextReportsDryRun(t *testing.T) {
	path := saveTestConfig(t, fileBackendConfig(t))
	seedFileBackend(t, "home", map[string]string{credentials.GitTokenKey: "home-token"})
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"config", "clear", "--dry-run"}); err != nil {
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
	assertFileBackendPresent(t, "home", credentials.GitTokenKey)
}

func TestConfigClearAllClearsOnlyDefaultProfileAndRemovesCache(t *testing.T) {
	cfg := fileBackendConfig(t)
	alpha := cfg.Profiles["home"]
	alpha.Git.CredentialRef = "codereview/alpha"
	cfg.Profiles["alpha"] = alpha
	beta := cfg.Profiles["home"]
	beta.Git.CredentialRef = "codereview/beta"
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

	if err := root.Execute(cmd, []string{"config", "clear", "--all", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.ConfigClear
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.Backend != "file" || got.BackendSource != "config" {
		t.Fatalf("backend = (%q,%q), want (file,config)", got.Backend, got.BackendSource)
	}
	if len(got.Cleared) != 1 || got.Cleared[0].Ref != "codereview/home" {
		t.Fatalf("cleared = %#v, want default home ref only", got.Cleared)
	}
	if got.ConfigProfileRemoved != "home" || got.DefaultProfile != "alpha" {
		t.Fatalf("config clear fields = profile:%q default:%q, want home/alpha", got.ConfigProfileRemoved, got.DefaultProfile)
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
	if cfg.DefaultProfile != "alpha" {
		t.Fatalf("default_profile = %q, want alpha", cfg.DefaultProfile)
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
	path := saveTestConfig(t, fileBackendConfig(t))
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
	if got.ConfigProfileRemoved != "work" || got.DefaultProfile != "" || got.ConfigPathRemoved != "" {
		t.Fatalf("config dry-run fields = profile:%q default:%q path:%q, want work with unchanged default and retained path", got.ConfigProfileRemoved, got.DefaultProfile, got.ConfigPathRemoved)
	}
	if got.Cache == nil || got.Cache.Status != "would_remove" {
		t.Fatalf("cache = %#v, want would_remove", got.Cache)
	}
	wantRefs := []string{"codereview/work", "codereview/work-llm", "codereview/work-reviewer"}
	if len(got.Cleared) != len(wantRefs) {
		t.Fatalf("cleared = %#v, want %d refs", got.Cleared, len(wantRefs))
	}
	for i, want := range wantRefs {
		if got.Cleared[i].Ref != want || len(got.Cleared[i].Keys) != 1 {
			t.Fatalf("cleared[%d] = %#v, want ref %s with one key", i, got.Cleared[i], want)
		}
	}
	assertFileBackendPresent(t, "home", credentials.GitTokenKey)
	assertFileBackendPresent(t, "work", credentials.GitTokenKey)
	assertFileBackendPresent(t, "work-reviewer", credentials.GitTokenKey)
	assertFileBackendKeys(t, "work-llm", []string{credentials.AnthropicAPIKeyKey})
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

func TestConfigClearProfileAllClearsOnlySelectedProfile(t *testing.T) {
	path := saveTestConfig(t, fileBackendConfig(t))
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
	if got.ConfigProfileRemoved != "work" || got.DefaultProfile != "" {
		t.Fatalf("config clear fields = profile:%q default:%q, want removed work with unchanged default omitted", got.ConfigProfileRemoved, got.DefaultProfile)
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
	if cfg.DefaultProfile != "home" || len(cfg.Profiles) != 1 {
		t.Fatalf("remaining config = %#v, want home only", cfg)
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

func TestConfigClearProfileAllReselectsDefaultWhenSelectedProfileIsDefault(t *testing.T) {
	cfg := fileBackendConfig(t)
	alpha := cfg.Profiles["home"]
	alpha.Git.CredentialRef = "codereview/alpha"
	cfg.Profiles["alpha"] = alpha
	cfg.DefaultProfile = "work"
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
	if got.ConfigProfileRemoved != "work" || got.DefaultProfile != "alpha" {
		t.Fatalf("config clear fields = profile:%q default:%q, want work/alpha", got.ConfigProfileRemoved, got.DefaultProfile)
	}
	assertFileBackendMissing(t, "work", credentials.GitTokenKey)
	assertFileBackendPresent(t, "alpha", credentials.GitTokenKey)
	assertFileBackendPresent(t, "home", credentials.GitTokenKey)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load remaining config: %v", err)
	}
	if cfg.DefaultProfile != "alpha" || len(cfg.Profiles) != 2 {
		t.Fatalf("remaining config = %#v, want alpha default with alpha/home only", cfg)
	}
	if _, ok := cfg.Profiles["work"]; ok {
		t.Fatalf("work profile still present after --all: %#v", cfg.Profiles)
	}
}

func TestConfigClearAllSingleProfileRemovesConfigFileAndEmptyParent(t *testing.T) {
	cfg := fileBackendConfig(t)
	cfg.Profiles = map[string]config.Profile{"home": cfg.Profiles["home"]}
	cfg.DefaultProfile = "home"
	configHome := t.TempDir()
	path := saveTestConfigAt(t, filepath.Join(configHome, statepaths.AppDir, "config.yml"), cfg)
	configDir := filepath.Dir(path)
	cacheFile := writeCacheSentinel(t)
	dataFile := writeDataSentinel(t)
	seedFileBackend(t, "home", map[string]string{credentials.GitTokenKey: "home-token"})
	cmd, out := newTestCommand(path)

	if err := root.Execute(cmd, []string{"config", "clear", "--all", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.ConfigClear
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal JSON: %v\n%s", err, out.String())
	}
	if got.ConfigProfileRemoved != "home" || got.DefaultProfile != "" || got.ConfigPathRemoved != path {
		t.Fatalf("config clear fields = profile:%q default:%q path:%q, want removed home config", got.ConfigProfileRemoved, got.DefaultProfile, got.ConfigPathRemoved)
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

	err := root.Execute(cmd, []string{"config", "clear", "--all", "--json"})
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

	err := root.Execute(cmd, []string{"config", "clear", "--all"})
	if err == nil {
		t.Fatal("Execute error = nil, want cache cleanup failure")
	}
	got := out.String()
	for _, want := range []string{
		"Cleared credentials:",
		"Config profile removed: home",
		"Default profile: work",
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

	err := root.Execute(cmd, []string{"config", "clear", "--all", "--json"})
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

	err := root.Execute(cmd, []string{"config", "clear", "--all"})
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
	if cfg.DefaultProfile != "home" {
		t.Fatalf("default_profile after failed save = %q, want home", cfg.DefaultProfile)
	}
	if _, ok := cfg.Profiles["home"]; !ok {
		t.Fatalf("home profile missing from on-disk config after failed save: %#v", cfg.Profiles)
	}
}

func newTestCommand(path string) (*cobra.Command, *bytes.Buffer) {
	var out bytes.Buffer
	cmd, opts := root.NewCommandWithOptions(&root.Options{
		ConfigPath: path,
		Stdin:      strings.NewReader(""),
		Stdout:     &out,
		Stderr:     &out,
	})
	Register(cmd, opts)
	return cmd, &out
}

func fileBackendConfig(t *testing.T) config.File {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("CODEREVIEW_KEYRING_PASSPHRASE", "test-passphrase")
	cfg := testConfig()
	cfg.Keyring.Backend = "file"
	return cfg
}

func writeDataSentinel(t *testing.T) string {
	t.Helper()
	dataFile := filepath.Join(os.Getenv("XDG_DATA_HOME"), "codereview", "runs", "sentinel.txt")
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
		DefaultProfile: "home",
		Keyring:        config.KeyringConfig{Backend: "memory"},
		Profiles: map[string]config.Profile{
			"home": {
				Git: config.GitConfig{
					Host:          "github.com",
					AuthMode:      config.GitAuthModePAT,
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
					CredentialRef: "codereview/work",
				},
				ReviewerCredentials: &config.ReviewerCredentials{
					AuthMode:      config.GitAuthModePAT,
					CredentialRef: "codereview/work-reviewer",
				},
				LLM: config.LLMConfig{
					Provider:      config.LLMProviderAnthropic,
					Auth:          config.LLMAuthAPIKey,
					Adapter:       config.LLMAdapterAnthropicAPI,
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

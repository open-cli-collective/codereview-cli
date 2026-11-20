package configcmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

func TestPreflightJudgeConfigSetShowAndUnset(t *testing.T) {
	const judgeYAML = `schema_version: 1
provider:
  kind: system_one
  endpoint: https://inference.example.test/v1/systemone
  credential: {store: local-os, name: codereview/judge}
  auth_scheme: bearer
model: example-model
expected_model: example-model
limits: {max_state_bytes: 6144, max_request_bytes: 8192}
budget: {max_calls: 2, max_total_request_bytes: 16384}
timeout: 5s
inference_timeout: 10s
`
	configPath := filepath.Join(t.TempDir(), "config.yml")
	if err := config.Save(configPath, testConfig()); err != nil {
		t.Fatal(err)
	}
	judgeDir := t.TempDir()
	t.Chdir(judgeDir)
	const relativePath = "team judge.yml"
	if err := os.WriteFile(relativePath, []byte(judgeYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	judgePath := filepath.Join(judgeDir, relativePath)

	run := func(args ...string) string {
		t.Helper()
		cmd, out := newTestCommand(configPath)
		if err := root.Execute(cmd, append([]string{"--profile", "work", "config"}, args...)); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	if output := run("preflight", "judge", "set", relativePath); !strings.Contains(output, `profile "work"`) ||
		!strings.Contains(output, "inference.example.test") || !strings.Contains(output, "example-model") ||
		strings.Contains(output, "codereview/judge") {
		t.Fatalf("unexpected opt-in confirmation: %s", output)
	}
	run("retention", "set", "--max-age-days", "2")
	if output := run("show"); !strings.Contains(output, judgePath) {
		t.Fatalf("config show omitted saved judge path: %s", output)
	}
	var rendered view.ConfigShow
	if err := json.Unmarshal([]byte(run("show", "--json")), &rendered); err != nil {
		t.Fatal(err)
	}
	if rendered.Profile.Preflight.JudgeConfigPath != judgePath {
		t.Fatalf("config show JSON judge path = %q, want %q", rendered.Profile.Preflight.JudgeConfigPath, judgePath)
	}
	cfg, err := config.Load(configPath)
	if err != nil || cfg.Profiles["work"].Preflight.JudgeConfigPath != judgePath ||
		cfg.Profiles["home"].Preflight.JudgeConfigPath != "" {
		t.Fatalf("unexpected settings after configuring work: %+v, %v", cfg.Profiles, err)
	}

	if err := os.WriteFile("invalid.yml", []byte("schema_version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd, _ := newTestCommand(configPath)
	if err := root.Execute(cmd, []string{"--profile", "work", "config", "preflight", "judge", "set", "invalid.yml"}); err == nil {
		t.Fatal("setting an invalid judge config succeeded")
	}
	if output := run("show"); !strings.Contains(output, judgePath) {
		t.Fatalf("failed set replaced the previous binding: %s", output)
	}

	run("preflight", "judge", "unset")
	run("preflight", "judge", "unset")
	if output := run("show", "--json"); strings.Contains(output, "judge_config") {
		t.Fatalf("unset left saved judge in config: %s", output)
	}
}

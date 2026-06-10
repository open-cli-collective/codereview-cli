package benchmark

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/config"
)

func TestValidateAcceptsValidSuite(t *testing.T) {
	suite := loadSuite(t, validSuiteYAML())

	if err := Validate(suite, testConfig()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateAcceptsZeroNumericLimits(t *testing.T) {
	body := replaceSuiteLine(validSuiteYAML(), "    max_agents: 5", "    max_agents: 0")
	body = replaceSuiteLine(body, "    max_concurrency: 3", "    max_concurrency: 0")
	suite := loadSuite(t, body)

	if err := Validate(suite, testConfig()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateForRunRejectsStructuralOnlySelectionRecipe(t *testing.T) {
	body := strings.Replace(validSuiteYAML(), `      selection:
        model: gpt-5.4
        effort: high
        prompt: prompts/selection-v1.md
      reviewers:
        model: gpt-5.4
        effort: high
        agent_dirs:
          - .codereview/agents
`, `      selection:
        prompt: prompts/selection-v1.md
`, 1)
	suite := loadSuite(t, body)

	if err := Validate(suite, testConfig()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	err := ValidateForRun(suite, testConfig())
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "stages.selection.model is required for benchmark run") {
		t.Fatalf("ValidateForRun error = %v, want missing selection model", err)
	}
}

func TestValidateForSelectionAcceptsMissingReviewerStage(t *testing.T) {
	body := strings.Replace(validSuiteYAML(), `      reviewers:
        model: gpt-5.4
        effort: high
        agent_dirs:
          - .codereview/agents
`, "", 1)
	suite := loadSuite(t, body)

	if err := ValidateForSelection(suite, testConfig()); err != nil {
		t.Fatalf("ValidateForSelection: %v", err)
	}
}

func TestValidateForSelectionRejectsStructuralOnlySelectionRecipe(t *testing.T) {
	body := strings.Replace(validSuiteYAML(), `      selection:
        model: gpt-5.4
        effort: high
        prompt: prompts/selection-v1.md
      reviewers:
        model: gpt-5.4
        effort: high
        agent_dirs:
          - .codereview/agents
`, `      selection:
        prompt: prompts/selection-v1.md
`, 1)
	suite := loadSuite(t, body)

	if err := Validate(suite, testConfig()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	err := ValidateForSelection(suite, testConfig())
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "stages.selection.model is required for benchmark select") {
		t.Fatalf("ValidateForSelection error = %v, want missing selection model", err)
	}
}

func TestValidateRejectsMissingSelectionStage(t *testing.T) {
	body := strings.Replace(validSuiteYAML(), `      selection:
        model: gpt-5.4
        effort: high
        prompt: prompts/selection-v1.md
`, "", 1)
	suite := loadSuite(t, body)

	err := Validate(suite, testConfig())
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "stages.selection is required") {
		t.Fatalf("Validate error = %v, want missing selection stage rejection", err)
	}
}

func TestValidateForRunAcceptsEmptyReviewerAgentDirsField(t *testing.T) {
	body := strings.Replace(validSuiteYAML(), "        agent_dirs:\n          - .codereview/agents", "        agent_dirs: []", 1)
	suite := loadSuite(t, body)

	if err := ValidateForRun(suite, testConfig()); err != nil {
		t.Fatalf("ValidateForRun: %v", err)
	}
}

func TestValidateForRunAcceptsReviewerModelTierWithoutReviewerModel(t *testing.T) {
	body := strings.Replace(validSuiteYAML(), "      reviewers:\n        model: gpt-5.4\n        effort: high\n", "      reviewers:\n        model_tier: medium\n        effort: high\n", 1)
	suite := loadSuite(t, body)

	if err := ValidateForRun(suite, testConfig()); err != nil {
		t.Fatalf("ValidateForRun: %v", err)
	}
}

func TestValidateForRunRejectsMissingReviewerStage(t *testing.T) {
	body := strings.Replace(validSuiteYAML(), `      reviewers:
        model: gpt-5.4
        effort: high
        agent_dirs:
          - .codereview/agents
`, "", 1)
	suite := loadSuite(t, body)

	if err := Validate(suite, testConfig()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	err := ValidateForRun(suite, testConfig())
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "stages.reviewers is required for benchmark run") {
		t.Fatalf("ValidateForRun error = %v, want missing reviewer stage rejection", err)
	}
}

func TestValidateRejectsReviewerModelAndModelTierTogether(t *testing.T) {
	body := strings.Replace(validSuiteYAML(), "      reviewers:\n        model: gpt-5.4\n        effort: high\n", "      reviewers:\n        model: gpt-5.4\n        model_tier: medium\n        effort: high\n", 1)
	suite := loadSuite(t, body)

	err := Validate(suite, testConfig())
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "cannot set both model and model_tier") {
		t.Fatalf("Validate error = %v, want model/model_tier conflict", err)
	}
}

func TestValidateForRunRejectsMissingReviewerAgentDirsField(t *testing.T) {
	body := strings.Replace(validSuiteYAML(), "        agent_dirs:\n          - .codereview/agents\n", "", 1)
	suite := loadSuite(t, body)

	if err := Validate(suite, testConfig()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	err := ValidateForRun(suite, testConfig())
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "stages.reviewers.agent_dirs is required for benchmark run") {
		t.Fatalf("ValidateForRun error = %v, want missing reviewer agent_dirs rejection", err)
	}
}

func TestValidateForRunRejectsEmptySelectionPromptFile(t *testing.T) {
	dir := t.TempDir()
	suitePath := filepath.Join(dir, "suite.yml")
	promptPath := filepath.Join(dir, "selection.md")
	body := replaceSuiteLine(validSuiteYAML(), "        prompt: prompts/selection-v1.md", "        prompt: selection.md")

	if err := os.WriteFile(promptPath, []byte(" \n\t "), 0o600); err != nil {
		t.Fatalf("WriteFile prompt: %v", err)
	}
	if err := os.WriteFile(suitePath, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile suite: %v", err)
	}
	suite, err := LoadFile(suitePath)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	err = ValidateForRun(suite, testConfig())
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "must contain non-empty prompt text") {
		t.Fatalf("ValidateForRun error = %v, want empty selection prompt rejection", err)
	}
}

func TestValidateForRunAllowsSynthesisPromptWithoutReadableFile(t *testing.T) {
	dir := t.TempDir()
	suitePath := filepath.Join(dir, "suite.yml")
	selectionPromptPath := filepath.Join(dir, "prompts", "selection-v1.md")
	if err := os.MkdirAll(filepath.Dir(selectionPromptPath), 0o700); err != nil {
		t.Fatalf("MkdirAll prompts: %v", err)
	}
	if err := os.WriteFile(selectionPromptPath, []byte("selection prompt"), 0o600); err != nil {
		t.Fatalf("WriteFile selection prompt: %v", err)
	}
	body := withSynthesisStage(validSuiteYAML(), "prompts/synthesis-v1.md")
	if err := os.WriteFile(suitePath, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile suite: %v", err)
	}
	suite, err := LoadFile(suitePath)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	if err := ValidateForRun(suite, testConfig()); err != nil {
		t.Fatalf("ValidateForRun: %v", err)
	}
}

func TestLoadNormalizesAgentsDirAlias(t *testing.T) {
	suite := loadSuite(t, strings.Replace(validSuiteYAML(), "agent_dirs:", "agents_dir:", 1))

	if err := Validate(suite, testConfig()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(suite.Candidates[0].Stages.Reviewers.AgentDirs) != 1 || suite.Candidates[0].Stages.Reviewers.AgentDirs[0] != ".codereview/agents" {
		t.Fatalf("agent dirs = %#v, want alias normalized", suite.Candidates[0].Stages.Reviewers.AgentDirs)
	}
}

func TestLoadRejectsAgentDirAliasConflict(t *testing.T) {
	_, err := Load([]byte(`
suite:
  id: suite1
candidates:
  - id: cand1
    profile: home
    stages:
      selection:
        model: gpt-5.4
        effort: high
      reviewers:
        model: gpt-5.4
        effort: high
        agent_dirs: [agents]
        agents_dir: [other]
cases:
  - id: case1
    pr: https://github.com/open-cli-collective/codereview-cli/pull/1
`))
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "reviewers stage cannot set both agent_dirs and agents_dir") {
		t.Fatalf("Load error = %v, want alias conflict", err)
	}
}

func TestLoadRejectsScalarAgentDirs(t *testing.T) {
	_, err := Load([]byte(`
suite:
  id: suite1
candidates:
  - id: cand1
    profile: home
    stages:
      selection:
        model: gpt-5.4
        effort: high
      reviewers:
        model: gpt-5.4
        effort: high
        agent_dirs: agents
cases:
  - id: case1
    pr: https://github.com/open-cli-collective/codereview-cli/pull/1
`))
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "decode candidate") {
		t.Fatalf("Load error = %v, want scalar list rejection", err)
	}
}

func TestValidateRejectsInvalidSuites(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "missing suite id", body: replaceSuiteLine(validSuiteYAML(), "  id: suite1", `  id: "  "`), want: "suite id is required"},
		{name: "invalid suite id slash", body: replaceSuiteLine(validSuiteYAML(), "  id: suite1", "  id: bad/id"), want: "must match"},
		{name: "invalid suite id dot", body: replaceSuiteLine(validSuiteYAML(), "  id: suite1", "  id: bad.id"), want: "must match"},
		{name: "invalid suite id parent", body: replaceSuiteLine(validSuiteYAML(), "  id: suite1", "  id: .."), want: "must match"},
		{name: "invalid suite id space", body: replaceSuiteLine(validSuiteYAML(), "  id: suite1", "  id: bad id"), want: "must match"},
		{name: "missing candidates", body: `
suite:
  id: suite1
cases:
  - id: case1
    pr: https://github.com/open-cli-collective/codereview-cli/pull/1
`, want: "at least one candidate"},
		{name: "missing cases", body: `
suite:
  id: suite1
candidates:
  - id: cand1
    profile: home
`, want: "at least one case"},
		{name: "duplicate candidate after trim", body: `
suite:
  id: suite1
candidates:
  - id: cand1
    profile: home
    stages:
      selection:
        model: gpt-5.4
        effort: high
  - id: " cand1 "
    profile: home
    stages:
      selection:
        model: gpt-5.4
        effort: high
cases:
  - id: case1
    pr: https://github.com/open-cli-collective/codereview-cli/pull/1
`, want: "duplicate candidate"},
		{name: "duplicate case", body: `
suite:
  id: suite1
candidates:
  - id: cand1
    profile: home
    stages:
      selection:
        model: gpt-5.4
        effort: high
cases:
  - id: case1
    pr: https://github.com/open-cli-collective/codereview-cli/pull/1
  - id: case1
    pr: https://github.com/open-cli-collective/codereview-cli/pull/2
`, want: "duplicate case"},
		{name: "invalid PR", body: replaceSuiteLine(validSuiteYAML(), "    pr: https://github.com/open-cli-collective/codereview-cli/pull/1", "    pr: not-a-pr"), want: "invalid PR URL"},
		{name: "unknown profile", body: replaceSuiteLine(validSuiteYAML(), "    profile: home", "    profile: missing"), want: "unknown profile"},
		{name: "host mismatch", body: replaceSuiteLine(validSuiteYAML(), "    pr: https://github.com/open-cli-collective/codereview-cli/pull/1", "    pr: https://ghe.example/open-cli-collective/codereview-cli/pull/1"), want: "does not match"},
		{name: "negative max agents", body: replaceSuiteLine(validSuiteYAML(), "    max_agents: 5", "    max_agents: -1"), want: "max_agents"},
		{name: "negative max concurrency", body: replaceSuiteLine(validSuiteYAML(), "    max_concurrency: 3", "    max_concurrency: -1"), want: "max_concurrency"},
		{name: "blank selection model when present", body: replaceSuiteLine(validSuiteYAML(), "        model: gpt-5.4", `        model: "  "`), want: "stages.selection.model must be non-empty"},
		{name: "blank selection effort when present", body: replaceSuiteLine(validSuiteYAML(), "        effort: high", `        effort: "  "`), want: "stages.selection.effort must be non-empty"},
		{name: "missing synthesis model", body: withRawSynthesisStage(validSuiteYAML(), "      synthesis:\n        effort: low\n"), want: "stages.synthesis.model is required when stages.synthesis is set"},
		{name: "invalid synthesis effort", body: withRawSynthesisStage(validSuiteYAML(), "      synthesis:\n        model: gpt-5.4\n        effort: invalid\n"), want: "stages.synthesis.effort must be one of low, medium, high"},
		{name: "blank synthesis prompt", body: withRawSynthesisStage(validSuiteYAML(), "      synthesis:\n        model: gpt-5.4\n        effort: low\n        prompt: \"  \"\n"), want: "stages.synthesis.prompt must be non-empty when present"},
		{name: "invalid reviewer effort", body: strings.Replace(validSuiteYAML(), "        effort: high\n        agent_dirs:", "        effort: invalid\n        agent_dirs:", 1), want: "stages.reviewers.effort must be one of low, medium, high"},
		{name: "invalid review base sha", body: replaceSuiteLine(validSuiteYAML(), "    review_base_sha: 1111111", "    review_base_sha: notsha"), want: "review_base_sha"},
		{name: "blank review head sha", body: replaceSuiteLine(validSuiteYAML(), "    review_head_sha: 2222222", `    review_head_sha: "  "`), want: "review_head_sha must be non-empty"},
		{name: "missing review head sha", body: replaceSuiteLine(validSuiteYAML(), "    review_head_sha: 2222222\n", ""), want: "must be set together"},
		{name: "invalid expected sha", body: replaceSuiteLine(validSuiteYAML(), "    expected_base_sha: abc1234", "    expected_base_sha: notsha"), want: "expected_base_sha"},
		{name: "blank expected sha", body: replaceSuiteLine(validSuiteYAML(), "    expected_head_sha: def5678", `    expected_head_sha: "  "`), want: "expected_head_sha must be non-empty"},
		{name: "invalid anchor side", body: replaceSuiteLine(validSuiteYAML(), "        side: RIGHT", "        side: MIDDLE"), want: "side must be RIGHT or LEFT"},
		{name: "invalid anchor line count", body: replaceSuiteLine(validSuiteYAML(), "        lines: [2, 4]", "        lines: [2]"), want: "exactly two"},
		{name: "invalid anchor line order", body: replaceSuiteLine(validSuiteYAML(), "        lines: [2, 4]", "        lines: [4, 2]"), want: "positive and ordered"},
		{name: "invalid anchor zero line", body: replaceSuiteLine(validSuiteYAML(), "        lines: [2, 4]", "        lines: [0, 2]"), want: "positive and ordered"},
		{name: "invalid anchor negative line", body: replaceSuiteLine(validSuiteYAML(), "        lines: [2, 4]", "        lines: [-1, 2]"), want: "positive and ordered"},
		{name: "missing anchor file", body: replaceSuiteLine(validSuiteYAML(), "        file: internal/pipeline/pipeline.go", "        file: \"\""), want: "file is required"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			suite := loadSuite(t, tt.body)
			err := Validate(suite, testConfig())
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate error = %v, want ErrInvalid containing %q", err, tt.want)
			}
		})
	}
}

func TestLoadRejectsInvalidYAML(t *testing.T) {
	_, err := Load([]byte("suite:\n  id: ["))
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "parse suite YAML") {
		t.Fatalf("Load error = %v, want YAML parse error", err)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "root", body: validSuiteYAML() + "\nextra: true\n", want: `suite root unknown field "extra"`},
		{name: "suite", body: replaceSuiteLine(validSuiteYAML(), "  version: 1", "  version: 1\n  extra: true"), want: `suite unknown field "extra"`},
		{name: "candidate", body: replaceSuiteLine(validSuiteYAML(), "    stages:", "    model: gpt-5.4\n    stages:"), want: `candidate[0] unknown field "model"`},
		{name: "candidate effort", body: replaceSuiteLine(validSuiteYAML(), "    stages:", "    effort: high\n    stages:"), want: `candidate[0] unknown field "effort"`},
		{name: "candidate agent dirs", body: replaceSuiteLine(validSuiteYAML(), "    stages:", "    agent_dirs:\n      - .codereview/agents\n    stages:"), want: `candidate[0] unknown field "agent_dirs"`},
		{name: "candidate agents dir alias", body: replaceSuiteLine(validSuiteYAML(), "    stages:", "    agents_dir:\n      - .codereview/agents\n    stages:"), want: `candidate[0] unknown field "agents_dir"`},
		{name: "candidate model tier", body: replaceSuiteLine(validSuiteYAML(), "    stages:", "    model_tier: medium\n    stages:"), want: `candidate[0] unknown field "model_tier"`},
		{name: "nested selection", body: replaceSuiteLine(validSuiteYAML(), "        prompt: prompts/selection-v1.md", "        prompt_file: prompts/selection-v1.md"), want: `candidate[0] stages.selection unknown field "prompt_file"`},
		{name: "case", body: replaceSuiteLine(validSuiteYAML(), "    pr: https://github.com/open-cli-collective/codereview-cli/pull/1", "    pull_request: https://github.com/open-cli-collective/codereview-cli/pull/1"), want: `case[0] unknown field "pull_request"`},
		{name: "anchor", body: replaceSuiteLine(validSuiteYAML(), "        lines: [2, 4]", "        lines: [2, 4]\n        expected: true"), want: `case[0] anchor[0] unknown field "expected"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load([]byte(tt.body))
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load error = %v, want ErrInvalid containing %q", err, tt.want)
			}
		})
	}
}

func TestSelectPreservesSuiteOrderAndRejectsUnknownIDs(t *testing.T) {
	suite := loadSuite(t, `
suite:
  id: suite1
candidates:
  - id: first
    profile: home
  - id: second
    profile: home
cases:
  - id: one
    pr: https://github.com/open-cli-collective/codereview-cli/pull/1
  - id: two
    pr: https://github.com/open-cli-collective/codereview-cli/pull/2
`)

	candidates, cases, err := Select(suite, []string{"second", "first"}, []string{"two", "one"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if candidates[0].ID != "first" || candidates[1].ID != "second" || cases[0].ID != "one" || cases[1].ID != "two" {
		t.Fatalf("selection order = candidates %#v cases %#v, want suite order", candidates, cases)
	}
	if _, _, err := Select(suite, []string{"missing"}, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Select unknown candidate error = %v, want ErrInvalid", err)
	}
	if _, _, err := Select(suite, nil, []string{"missing"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Select unknown case error = %v, want ErrInvalid", err)
	}
}

func TestSelectRejectsInvalidFilters(t *testing.T) {
	suite := loadSuite(t, validSuiteYAML())
	tests := []struct {
		name       string
		candidate  []string
		benchCases []string
	}{
		{name: "empty candidate", candidate: []string{""}},
		{name: "blank candidate", candidate: []string{"  "}},
		{name: "unsafe candidate", candidate: []string{"bad/id"}},
		{name: "empty case", benchCases: []string{""}},
		{name: "unsafe case", benchCases: []string{"bad/id"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Select(suite, tt.candidate, tt.benchCases)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Select error = %v, want ErrInvalid", err)
			}
		})
	}
}

func loadSuite(t *testing.T, body string) SuiteFile {
	t.Helper()
	suite, err := Load([]byte(body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return suite
}

func validSuiteYAML() string {
	return `
suite:
  id: suite1
  name: Suite One
  version: 1
candidates:
  - id: cand1
    profile: home
    stages:
      selection:
        model: gpt-5.4
        effort: high
        prompt: prompts/selection-v1.md
      reviewers:
        model: gpt-5.4
        effort: high
        agent_dirs:
          - .codereview/agents
    max_agents: 5
    max_concurrency: 3
cases:
  - id: case1
    pr: https://github.com/open-cli-collective/codereview-cli/pull/1
    review_base_sha: 1111111
    review_head_sha: 2222222
    expected_base_sha: abc1234
    expected_head_sha: def5678
    anchors:
      - id: auth_check
        file: internal/pipeline/pipeline.go
        side: RIGHT
        lines: [2, 4]
`
}

func replaceSuiteLine(body, old, replacement string) string {
	return strings.Replace(body, old, replacement, 1)
}

func withSynthesisStage(body, prompt string) string {
	stage := "      synthesis:\n        model: gpt-5.4\n        effort: low\n"
	if prompt != "" {
		stage += "        prompt: " + prompt + "\n"
	}
	return withRawSynthesisStage(body, stage)
}

func withRawSynthesisStage(body, stage string) string {
	return strings.Replace(body, "    max_agents: 5\n", stage+"    max_agents: 5\n", 1)
}

func testConfig() config.File {
	return config.File{
		DefaultProfile: "home",
		Profiles: map[string]config.Profile{
			"home": {
				Git: config.GitConfig{
					Host: "github.com",
				},
			},
		},
	}
}

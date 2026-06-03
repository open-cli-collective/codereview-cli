package benchmark

import (
	"errors"
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

func TestLoadNormalizesAgentsDirAlias(t *testing.T) {
	suite := loadSuite(t, strings.Replace(validSuiteYAML(), "agent_dirs:", "agents_dir:", 1))

	if err := Validate(suite, testConfig()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(suite.Candidates[0].AgentDirs) != 1 || suite.Candidates[0].AgentDirs[0] != ".codereview/agents" {
		t.Fatalf("agent dirs = %#v, want alias normalized", suite.Candidates[0].AgentDirs)
	}
}

func TestLoadRejectsAgentDirAliasConflict(t *testing.T) {
	_, err := Load([]byte(`
suite:
  id: suite1
candidates:
  - id: cand1
    profile: home
    agent_dirs: [agents]
    agents_dir: [other]
cases:
  - id: case1
    pr: https://github.com/open-cli-collective/codereview-cli/pull/1
`))
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "cannot set both agent_dirs and agents_dir") {
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
  - id: " cand1 "
    profile: home
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
		{name: "blank model when present", body: replaceSuiteLine(validSuiteYAML(), "    model: gpt-5.1", `    model: "  "`), want: "model must be non-empty"},
		{name: "blank effort when present", body: replaceSuiteLine(validSuiteYAML(), "    effort: high", `    effort: "  "`), want: "effort must be non-empty"},
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
		{name: "candidate", body: replaceSuiteLine(validSuiteYAML(), "    model: gpt-5.1", "    model_name: gpt-5.1"), want: `candidate[0] unknown field "model_name"`},
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
    model: gpt-5.1
    effort: high
    agent_dirs:
      - .codereview/agents
    max_agents: 5
    max_concurrency: 3
cases:
  - id: case1
    pr: https://github.com/open-cli-collective/codereview-cli/pull/1
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

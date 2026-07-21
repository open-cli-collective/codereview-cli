// Package benchmark parses and validates code-review benchmark suites.
package benchmark

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/modelprefs"
	"github.com/open-cli-collective/codereview-cli/internal/prref"
)

var (
	// ErrInvalid means a benchmark suite is malformed or violates the schema.
	ErrInvalid = errors.New("benchmark: invalid")

	idPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
	shaPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)
)

// SuiteFile is one benchmark suite document.
type SuiteFile struct {
	Path       string      `yaml:"-" json:"-"`
	Suite      Suite       `yaml:"suite" json:"suite"`
	Candidates []Candidate `yaml:"candidates" json:"candidates"`
	Cases      []Case      `yaml:"cases" json:"cases"`
}

// Suite identifies a collection of benchmark cases and candidates.
type Suite struct {
	ID      string `yaml:"id" json:"id"`
	Name    string `yaml:"name,omitempty" json:"name,omitempty"`
	Version int    `yaml:"version,omitempty" json:"version,omitempty"`
}

// Candidate is one review configuration to try against each selected case.
type Candidate struct {
	ID             string          `yaml:"id" json:"id"`
	Profile        string          `yaml:"profile" json:"profile"`
	Stages         CandidateStages `yaml:"stages" json:"stages"`
	MaxAgents      int             `yaml:"max_agents,omitempty" json:"max_agents,omitempty"`
	MaxConcurrency int             `yaml:"max_concurrency,omitempty" json:"max_concurrency,omitempty"`

	stagesSet bool
}

// CandidateStages groups the per-phase runtime recipes for a benchmark candidate.
type CandidateStages struct {
	Selection SelectionStage `yaml:"selection" json:"selection"`
	Reviewers ReviewerStage  `yaml:"reviewers,omitempty" json:"reviewers,omitempty"`
	Synthesis SelectionStage `yaml:"synthesis,omitempty" json:"synthesis,omitempty"`

	selectionSet bool
	reviewersSet bool
	synthesisSet bool
}

// SelectionStage configures the benchmark selection/orchestration phase.
type SelectionStage struct {
	Model  string `yaml:"model,omitempty" json:"model,omitempty"`
	Effort string `yaml:"effort,omitempty" json:"effort,omitempty"`
	Prompt string `yaml:"prompt,omitempty" json:"prompt,omitempty"`

	modelSet  bool
	effortSet bool
	promptSet bool
}

// ReviewerStage configures the benchmark reviewer execution phase.
type ReviewerStage struct {
	Model     string   `yaml:"model,omitempty" json:"model,omitempty"`
	ModelTier string   `yaml:"model_tier,omitempty" json:"model_tier,omitempty"`
	Effort    string   `yaml:"effort,omitempty" json:"effort,omitempty"`
	AgentDirs []string `yaml:"agent_dirs,omitempty" json:"agent_dirs,omitempty"`

	modelTierSet bool
	modelSet     bool
	effortSet    bool
	agentDirsSet bool
}

// UnmarshalYAML tracks candidate field presence for validation.
func (c *Candidate) UnmarshalYAML(value *yaml.Node) error {
	type rawCandidate struct {
		ID             string          `yaml:"id"`
		Profile        string          `yaml:"profile"`
		Stages         CandidateStages `yaml:"stages"`
		MaxAgents      int             `yaml:"max_agents"`
		MaxConcurrency int             `yaml:"max_concurrency"`
	}
	var raw rawCandidate
	if err := value.Decode(&raw); err != nil {
		return fmt.Errorf("%w: decode candidate: %w", ErrInvalid, err)
	}
	*c = Candidate{
		ID:             raw.ID,
		Profile:        raw.Profile,
		Stages:         raw.Stages,
		MaxAgents:      raw.MaxAgents,
		MaxConcurrency: raw.MaxConcurrency,
		stagesSet:      mappingHasKey(value, "stages"),
	}
	return nil
}

// UnmarshalYAML tracks per-stage field presence for validation.
func (s *CandidateStages) UnmarshalYAML(value *yaml.Node) error {
	type rawStages struct {
		Selection SelectionStage `yaml:"selection"`
		Reviewers ReviewerStage  `yaml:"reviewers"`
		Synthesis SelectionStage `yaml:"synthesis"`
	}
	var raw rawStages
	if err := value.Decode(&raw); err != nil {
		return fmt.Errorf("%w: decode stages: %w", ErrInvalid, err)
	}
	*s = CandidateStages{
		Selection:    raw.Selection,
		Reviewers:    raw.Reviewers,
		Synthesis:    raw.Synthesis,
		selectionSet: mappingHasKey(value, "selection"),
		reviewersSet: mappingHasKey(value, "reviewers"),
		synthesisSet: mappingHasKey(value, "synthesis"),
	}
	return nil
}

// UnmarshalYAML tracks selection stage field presence for validation.
func (s *SelectionStage) UnmarshalYAML(value *yaml.Node) error {
	type rawSelectionStage struct {
		Model  string `yaml:"model"`
		Effort string `yaml:"effort"`
		Prompt string `yaml:"prompt"`
	}
	var raw rawSelectionStage
	if err := value.Decode(&raw); err != nil {
		return fmt.Errorf("%w: decode selection stage: %w", ErrInvalid, err)
	}
	*s = SelectionStage{
		Model:     raw.Model,
		Effort:    raw.Effort,
		Prompt:    raw.Prompt,
		modelSet:  mappingHasKey(value, "model"),
		effortSet: mappingHasKey(value, "effort"),
		promptSet: mappingHasKey(value, "prompt"),
	}
	return nil
}

// UnmarshalYAML accepts the canonical agent_dirs field and the draft agents_dir
// alias inside reviewer stage recipes, but rejects documents that use both.
func (s *ReviewerStage) UnmarshalYAML(value *yaml.Node) error {
	type rawReviewerStage struct {
		Model     string   `yaml:"model"`
		ModelTier string   `yaml:"model_tier"`
		Effort    string   `yaml:"effort"`
		AgentDirs []string `yaml:"agent_dirs"`
		AgentsDir []string `yaml:"agents_dir"`
	}
	var raw rawReviewerStage
	if err := value.Decode(&raw); err != nil {
		return fmt.Errorf("%w: decode reviewers stage: %w", ErrInvalid, err)
	}
	hasAgentDirs := mappingHasKey(value, "agent_dirs")
	hasAgentsDir := mappingHasKey(value, "agents_dir")
	if hasAgentDirs && hasAgentsDir {
		return fmt.Errorf("%w: reviewers stage cannot set both agent_dirs and agents_dir", ErrInvalid)
	}
	agentDirs := raw.AgentDirs
	if hasAgentsDir {
		agentDirs = raw.AgentsDir
	}
	*s = ReviewerStage{
		Model:        raw.Model,
		ModelTier:    raw.ModelTier,
		Effort:       raw.Effort,
		AgentDirs:    agentDirs,
		modelSet:     mappingHasKey(value, "model"),
		modelTierSet: mappingHasKey(value, "model_tier"),
		effortSet:    mappingHasKey(value, "effort"),
		agentDirsSet: hasAgentDirs || hasAgentsDir,
	}
	return nil
}

// Case is one pull request to review during a benchmark.
type Case struct {
	ID              string   `yaml:"id" json:"id"`
	PR              string   `yaml:"pr" json:"pr"`
	ReviewBaseSHA   string   `yaml:"review_base_sha,omitempty" json:"review_base_sha,omitempty"`
	ReviewHeadSHA   string   `yaml:"review_head_sha,omitempty" json:"review_head_sha,omitempty"`
	ExpectedBaseSHA string   `yaml:"expected_base_sha,omitempty" json:"expected_base_sha,omitempty"`
	ExpectedHeadSHA string   `yaml:"expected_head_sha,omitempty" json:"expected_head_sha,omitempty"`
	Anchors         []Anchor `yaml:"anchors,omitempty" json:"anchors,omitempty"`

	reviewBaseSHASet   bool
	reviewHeadSHASet   bool
	expectedBaseSHASet bool
	expectedHeadSHASet bool
}

// UnmarshalYAML tracks optional SHA field presence so explicitly blank values
// can be rejected after normalization.
func (c *Case) UnmarshalYAML(value *yaml.Node) error {
	type rawCase struct {
		ID              string   `yaml:"id"`
		PR              string   `yaml:"pr"`
		ReviewBaseSHA   string   `yaml:"review_base_sha"`
		ReviewHeadSHA   string   `yaml:"review_head_sha"`
		ExpectedBaseSHA string   `yaml:"expected_base_sha"`
		ExpectedHeadSHA string   `yaml:"expected_head_sha"`
		Anchors         []Anchor `yaml:"anchors"`
	}
	var raw rawCase
	if err := value.Decode(&raw); err != nil {
		return fmt.Errorf("%w: decode case: %w", ErrInvalid, err)
	}
	*c = Case{
		ID:                 raw.ID,
		PR:                 raw.PR,
		ReviewBaseSHA:      raw.ReviewBaseSHA,
		ReviewHeadSHA:      raw.ReviewHeadSHA,
		ExpectedBaseSHA:    raw.ExpectedBaseSHA,
		ExpectedHeadSHA:    raw.ExpectedHeadSHA,
		Anchors:            raw.Anchors,
		reviewBaseSHASet:   mappingHasKey(value, "review_base_sha"),
		reviewHeadSHASet:   mappingHasKey(value, "review_head_sha"),
		expectedBaseSHASet: mappingHasKey(value, "expected_base_sha"),
		expectedHeadSHASet: mappingHasKey(value, "expected_head_sha"),
	}
	return nil
}

// Anchor is optional non-scoring placement metadata for a case.
type Anchor struct {
	ID    string `yaml:"id" json:"id"`
	File  string `yaml:"file" json:"file"`
	Side  string `yaml:"side" json:"side"`
	Lines []int  `yaml:"lines" json:"lines"`
}

// LoadFile reads and parses a benchmark suite file.
func LoadFile(path string) (SuiteFile, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- caller-selected suite path is the command input.
	if err != nil {
		return SuiteFile{}, fmt.Errorf("benchmark: read suite: %w", err)
	}
	suite, err := Load(data)
	if err != nil {
		return SuiteFile{}, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return SuiteFile{}, fmt.Errorf("benchmark: resolve suite path: %w", err)
	}
	suite.Path = abs
	return suite, nil
}

// Load parses a benchmark suite document.
func Load(data []byte) (SuiteFile, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return SuiteFile{}, fmt.Errorf("%w: parse suite YAML: %w", ErrInvalid, err)
	}
	if err := validateKnownFields(&root); err != nil {
		return SuiteFile{}, err
	}
	var suite SuiteFile
	if err := root.Decode(&suite); err != nil {
		return SuiteFile{}, fmt.Errorf("%w: parse suite YAML: %w", ErrInvalid, err)
	}
	Normalize(&suite)
	return suite, nil
}

// Normalize trims user-facing scalar fields in a suite document.
func Normalize(suite *SuiteFile) {
	trimAll(&suite.Suite.ID, &suite.Suite.Name)
	for i := range suite.Candidates {
		c := &suite.Candidates[i]
		trimAll(
			&c.ID, &c.Profile,
			&c.Stages.Selection.Model, &c.Stages.Selection.Effort, &c.Stages.Selection.Prompt,
			&c.Stages.Synthesis.Model, &c.Stages.Synthesis.Effort, &c.Stages.Synthesis.Prompt,
			&c.Stages.Reviewers.Model, &c.Stages.Reviewers.ModelTier, &c.Stages.Reviewers.Effort,
		)
		for j := range c.Stages.Reviewers.AgentDirs {
			trimAll(&c.Stages.Reviewers.AgentDirs[j])
		}
	}
	for i := range suite.Cases {
		c := &suite.Cases[i]
		trimAll(&c.ID, &c.PR, &c.ReviewBaseSHA, &c.ReviewHeadSHA, &c.ExpectedBaseSHA, &c.ExpectedHeadSHA)
		for j := range c.Anchors {
			a := &c.Anchors[j]
			trimAll(&a.ID, &a.File, &a.Side)
		}
	}
}

func trimAll(fields ...*string) {
	for _, field := range fields {
		*field = strings.TrimSpace(*field)
	}
}

// Validate checks suite schema and profile/case compatibility without assuming a
// specific benchmark command mode.
func Validate(suite SuiteFile, cfg config.File) error {
	Normalize(&suite)
	if err := validateID("suite id", suite.Suite.ID); err != nil {
		return err
	}
	if suite.Suite.Version < 0 {
		return fmt.Errorf("%w: suite version must be non-negative", ErrInvalid)
	}
	if len(suite.Candidates) == 0 {
		return fmt.Errorf("%w: at least one candidate is required", ErrInvalid)
	}
	if len(suite.Cases) == 0 {
		return fmt.Errorf("%w: at least one case is required", ErrInvalid)
	}
	if err := validateCandidates(suite.Candidates, cfg); err != nil {
		return err
	}
	if err := validateCases(suite.Cases); err != nil {
		return err
	}
	return validateCandidateCaseHosts(suite.Candidates, suite.Cases, cfg)
}

// ValidateForRun applies the stricter full-pipeline requirements used by the
// current benchmark validate/doctor/run commands.
func ValidateForRun(suite SuiteFile, cfg config.File) error {
	if err := Validate(suite, cfg); err != nil {
		return err
	}
	return validateRunCandidates(suite)
}

// ValidateForSelection applies the stricter selector-only requirements used by
// benchmark select.
func ValidateForSelection(suite SuiteFile, cfg config.File) error {
	if err := Validate(suite, cfg); err != nil {
		return err
	}
	return validateSelectionCandidates(suite)
}

// Select returns suite-order candidates and cases after optional ID filtering.
func Select(suite SuiteFile, candidateIDs, caseIDs []string) ([]Candidate, []Case, error) {
	selectedCandidates, err := filterByID("candidate", suite.Candidates, candidateIDs, func(candidate Candidate) string { return candidate.ID })
	if err != nil {
		return nil, nil, err
	}
	selectedCases, err := filterByID("case", suite.Cases, caseIDs, func(benchCase Case) string { return benchCase.ID })
	if err != nil {
		return nil, nil, err
	}
	return selectedCandidates, selectedCases, nil
}

func filterByID[T any](kind string, values []T, ids []string, idOf func(T) string) ([]T, error) {
	filter, err := filterIDSet(kind, ids)
	if err != nil {
		return nil, err
	}
	selected := make([]T, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		id := idOf(value)
		if len(filter) > 0 && !filter[id] {
			continue
		}
		selected = append(selected, value)
		seen[id] = true
	}
	for id := range filter {
		if !seen[id] {
			return nil, fmt.Errorf("%w: unknown %s %q", ErrInvalid, kind, id)
		}
	}
	return selected, nil
}

func validateCandidates(candidates []Candidate, cfg config.File) error {
	seen := map[string]bool{}
	for i, candidate := range candidates {
		if err := validateID(fmt.Sprintf("candidate[%d] id", i), candidate.ID); err != nil {
			return err
		}
		if seen[candidate.ID] {
			return fmt.Errorf("%w: duplicate candidate id %q", ErrInvalid, candidate.ID)
		}
		seen[candidate.ID] = true
		if candidate.Profile == "" {
			return fmt.Errorf("%w: candidate %q profile is required", ErrInvalid, candidate.ID)
		}
		if _, ok := cfg.Profiles[candidate.Profile]; !ok {
			return fmt.Errorf("%w: candidate %q references unknown profile %q", ErrInvalid, candidate.ID, candidate.Profile)
		}
		if !candidate.stagesSet {
			return fmt.Errorf("%w: candidate %q stages is required", ErrInvalid, candidate.ID)
		}
		if err := validateSelectionStage(candidate.ID, candidate.Stages); err != nil {
			return err
		}
		if err := validateReviewerStageStructural(candidate.ID, candidate.Stages); err != nil {
			return err
		}
		if err := validateSynthesisStageStructural(candidate.ID, candidate.Stages); err != nil {
			return err
		}
		if candidate.MaxAgents < 0 {
			return fmt.Errorf("%w: candidate %q max_agents must be non-negative", ErrInvalid, candidate.ID)
		}
		if candidate.MaxConcurrency < 0 {
			return fmt.Errorf("%w: candidate %q max_concurrency must be non-negative", ErrInvalid, candidate.ID)
		}
	}
	return nil
}

func validateSelectionStage(candidateID string, stages CandidateStages) error {
	if !stages.selectionSet {
		return fmt.Errorf("%w: candidate %q stages.selection is required", ErrInvalid, candidateID)
	}
	if stages.Selection.modelSet && stages.Selection.Model == "" {
		return fmt.Errorf("%w: candidate %q stages.selection.model must be non-empty when present", ErrInvalid, candidateID)
	}
	if stages.Selection.effortSet && stages.Selection.Effort == "" {
		return fmt.Errorf("%w: candidate %q stages.selection.effort must be non-empty when present", ErrInvalid, candidateID)
	}
	if stages.Selection.promptSet && stages.Selection.Prompt == "" {
		return fmt.Errorf("%w: candidate %q stages.selection.prompt must be non-empty when present", ErrInvalid, candidateID)
	}
	if stages.Selection.Effort != "" && !modelprefs.Effort(stages.Selection.Effort).Valid() {
		return fmt.Errorf("%w: candidate %q stages.selection.effort must be one of low, medium, high", ErrInvalid, candidateID)
	}
	return nil
}

func validateReviewerStageStructural(candidateID string, stages CandidateStages) error {
	if !stages.reviewersSet {
		return nil
	}
	if stages.Reviewers.modelSet && stages.Reviewers.Model == "" {
		return fmt.Errorf("%w: candidate %q stages.reviewers.model must be non-empty when present", ErrInvalid, candidateID)
	}
	if stages.Reviewers.modelTierSet && stages.Reviewers.ModelTier == "" {
		return fmt.Errorf("%w: candidate %q stages.reviewers.model_tier must be non-empty when present", ErrInvalid, candidateID)
	}
	if stages.Reviewers.Model != "" && stages.Reviewers.ModelTier != "" {
		return fmt.Errorf("%w: candidate %q stages.reviewers cannot set both model and model_tier", ErrInvalid, candidateID)
	}
	if stages.Reviewers.ModelTier != "" && !config.ModelTier(stages.Reviewers.ModelTier).Valid() {
		return fmt.Errorf("%w: candidate %q stages.reviewers.model_tier must be one of small, medium, large", ErrInvalid, candidateID)
	}
	if stages.Reviewers.effortSet && stages.Reviewers.Effort == "" {
		return fmt.Errorf("%w: candidate %q stages.reviewers.effort must be non-empty when present", ErrInvalid, candidateID)
	}
	if stages.Reviewers.Effort != "" && !modelprefs.Effort(stages.Reviewers.Effort).Valid() {
		return fmt.Errorf("%w: candidate %q stages.reviewers.effort must be one of low, medium, high", ErrInvalid, candidateID)
	}
	if !stages.Reviewers.agentDirsSet {
		return nil
	}
	for i, dir := range stages.Reviewers.AgentDirs {
		if dir == "" {
			return fmt.Errorf("%w: candidate %q stages.reviewers.agent_dirs[%d] must be non-empty", ErrInvalid, candidateID, i)
		}
	}
	return nil
}

func validateSynthesisStageStructural(candidateID string, stages CandidateStages) error {
	if !stages.synthesisSet {
		return nil
	}
	if stages.Synthesis.Model == "" {
		return fmt.Errorf("%w: candidate %q stages.synthesis.model is required when stages.synthesis is set", ErrInvalid, candidateID)
	}
	if stages.Synthesis.Effort == "" {
		return fmt.Errorf("%w: candidate %q stages.synthesis.effort is required when stages.synthesis is set", ErrInvalid, candidateID)
	}
	if stages.Synthesis.promptSet && stages.Synthesis.Prompt == "" {
		return fmt.Errorf("%w: candidate %q stages.synthesis.prompt must be non-empty when present", ErrInvalid, candidateID)
	}
	if !modelprefs.Effort(stages.Synthesis.Effort).Valid() {
		return fmt.Errorf("%w: candidate %q stages.synthesis.effort must be one of low, medium, high", ErrInvalid, candidateID)
	}
	return nil
}

func validateRunCandidates(suite SuiteFile) error {
	suiteDir := filepath.Dir(suite.Path)
	for _, candidate := range suite.Candidates {
		if err := validateRequiredSelectionRecipe(candidate.ID, candidate.Stages.Selection, suiteDir, suite.Path, "benchmark run"); err != nil {
			return err
		}
		if !candidate.Stages.reviewersSet {
			return fmt.Errorf("%w: candidate %q stages.reviewers is required for benchmark run", ErrInvalid, candidate.ID)
		}
		if candidate.Stages.Reviewers.Model == "" && candidate.Stages.Reviewers.ModelTier == "" {
			return fmt.Errorf("%w: candidate %q stages.reviewers.model or stages.reviewers.model_tier is required for benchmark run", ErrInvalid, candidate.ID)
		}
		if candidate.Stages.Reviewers.Effort == "" {
			return fmt.Errorf("%w: candidate %q stages.reviewers.effort is required for benchmark run", ErrInvalid, candidate.ID)
		}
		if !candidate.Stages.Reviewers.agentDirsSet {
			return fmt.Errorf("%w: candidate %q stages.reviewers.agent_dirs is required for benchmark run", ErrInvalid, candidate.ID)
		}
	}
	return nil
}

func validateSelectionCandidates(suite SuiteFile) error {
	suiteDir := filepath.Dir(suite.Path)
	for _, candidate := range suite.Candidates {
		if err := validateRequiredSelectionRecipe(candidate.ID, candidate.Stages.Selection, suiteDir, suite.Path, "benchmark select"); err != nil {
			return err
		}
	}
	return nil
}

func validateRequiredSelectionRecipe(candidateID string, selection SelectionStage, suiteDir, suitePath, commandName string) error {
	if selection.Model == "" {
		return fmt.Errorf("%w: candidate %q stages.selection.model is required for %s", ErrInvalid, candidateID, commandName)
	}
	if selection.Effort == "" {
		return fmt.Errorf("%w: candidate %q stages.selection.effort is required for %s", ErrInvalid, candidateID, commandName)
	}
	if prompt := selection.Prompt; prompt != "" && suitePath != "" {
		if err := validateSelectionPromptFile(candidateID, suiteDir, prompt); err != nil {
			return err
		}
	}
	return nil
}

// ResolveSuitePath resolves a slash-separated path relative to a benchmark suite directory.
func ResolveSuitePath(baseDir, path string) string {
	path = filepath.FromSlash(path)
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(baseDir, path)
}

func validateSelectionPromptFile(candidateID, suiteDir, prompt string) error {
	resolved := ResolveSuitePath(suiteDir, prompt)
	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("%w: candidate %q stages.selection.prompt %q must reference a readable file relative to the suite", ErrInvalid, candidateID, prompt)
	}
	if info.IsDir() {
		return fmt.Errorf("%w: candidate %q stages.selection.prompt %q must reference a file, not a directory", ErrInvalid, candidateID, prompt)
	}
	data, err := os.ReadFile(resolved) // #nosec G304 -- suite-selected prompt path is explicit benchmark input.
	if err != nil {
		return fmt.Errorf("%w: candidate %q stages.selection.prompt %q must reference a readable file relative to the suite", ErrInvalid, candidateID, prompt)
	}
	if strings.TrimSpace(string(data)) == "" {
		return fmt.Errorf("%w: candidate %q stages.selection.prompt %q must contain non-empty prompt text", ErrInvalid, candidateID, prompt)
	}
	return nil
}

func validateCases(cases []Case) error {
	seen := map[string]bool{}
	for i, benchCase := range cases {
		if err := validateID(fmt.Sprintf("case[%d] id", i), benchCase.ID); err != nil {
			return err
		}
		if seen[benchCase.ID] {
			return fmt.Errorf("%w: duplicate case id %q", ErrInvalid, benchCase.ID)
		}
		seen[benchCase.ID] = true
		if _, _, err := prref.ParsePullURL(benchCase.PR); err != nil {
			return fmt.Errorf("%w: case %q invalid PR URL: %w", ErrInvalid, benchCase.ID, err)
		}
		if err := validateOptionalSHA("review_base_sha", benchCase.ID, benchCase.ReviewBaseSHA, benchCase.reviewBaseSHASet); err != nil {
			return err
		}
		if err := validateOptionalSHA("review_head_sha", benchCase.ID, benchCase.ReviewHeadSHA, benchCase.reviewHeadSHASet); err != nil {
			return err
		}
		if (benchCase.ReviewBaseSHA == "") != (benchCase.ReviewHeadSHA == "") {
			return fmt.Errorf("%w: case %q review_base_sha and review_head_sha must be set together", ErrInvalid, benchCase.ID)
		}
		if err := validateOptionalSHA("expected_base_sha", benchCase.ID, benchCase.ExpectedBaseSHA, benchCase.expectedBaseSHASet); err != nil {
			return err
		}
		if err := validateOptionalSHA("expected_head_sha", benchCase.ID, benchCase.ExpectedHeadSHA, benchCase.expectedHeadSHASet); err != nil {
			return err
		}
		if err := validateAnchors(benchCase.ID, benchCase.Anchors); err != nil {
			return err
		}
	}
	return nil
}

func validateAnchors(caseID string, anchors []Anchor) error {
	seen := map[string]bool{}
	for i, anchor := range anchors {
		if err := validateID(fmt.Sprintf("case %q anchor[%d] id", caseID, i), anchor.ID); err != nil {
			return err
		}
		if seen[anchor.ID] {
			return fmt.Errorf("%w: case %q duplicate anchor id %q", ErrInvalid, caseID, anchor.ID)
		}
		seen[anchor.ID] = true
		if anchor.File == "" {
			return fmt.Errorf("%w: case %q anchor %q file is required", ErrInvalid, caseID, anchor.ID)
		}
		if anchor.Side != "RIGHT" && anchor.Side != "LEFT" {
			return fmt.Errorf("%w: case %q anchor %q side must be RIGHT or LEFT", ErrInvalid, caseID, anchor.ID)
		}
		if len(anchor.Lines) != 2 {
			return fmt.Errorf("%w: case %q anchor %q lines must contain exactly two integers", ErrInvalid, caseID, anchor.ID)
		}
		if anchor.Lines[0] <= 0 || anchor.Lines[1] <= 0 || anchor.Lines[0] > anchor.Lines[1] {
			return fmt.Errorf("%w: case %q anchor %q lines must be positive and ordered", ErrInvalid, caseID, anchor.ID)
		}
	}
	return nil
}

func validateCandidateCaseHosts(candidates []Candidate, cases []Case, cfg config.File) error {
	hosts := make([]string, len(cases))
	parsed := make([]bool, len(cases))
	for _, candidate := range candidates {
		profile := cfg.Profiles[candidate.Profile]
		for i, benchCase := range cases {
			if !parsed[i] {
				ref, _, err := prref.ParsePullURL(benchCase.PR)
				if err != nil {
					return fmt.Errorf("%w: case %q invalid PR URL: %w", ErrInvalid, benchCase.ID, err)
				}
				hosts[i], parsed[i] = ref.Host, true
			}
			if !prref.SameHost(hosts[i], profile.Git.Host) {
				return fmt.Errorf("%w: candidate %q profile host %q does not match case %q PR host %q", ErrInvalid, candidate.ID, profile.Git.Host, benchCase.ID, hosts[i])
			}
		}
	}
	return nil
}

func validateID(label, id string) error {
	if id == "" {
		return fmt.Errorf("%w: %s is required", ErrInvalid, label)
	}
	if !idPattern.MatchString(id) {
		return fmt.Errorf("%w: %s %q must match %s", ErrInvalid, label, id, idPattern.String())
	}
	return nil
}

func validateOptionalSHA(field, caseID, sha string, present bool) error {
	if sha == "" {
		if present {
			return fmt.Errorf("%w: case %q %s must be non-empty when present", ErrInvalid, caseID, field)
		}
		return nil
	}
	if !shaPattern.MatchString(sha) {
		return fmt.Errorf("%w: case %q %s must be a 7-64 character hex SHA", ErrInvalid, caseID, field)
	}
	return nil
}

func mappingHasKey(node *yaml.Node, key string) bool {
	if node.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return true
		}
	}
	return false
}

func validateKnownFields(doc *yaml.Node) error {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		doc = doc.Content[0]
	}
	if doc == nil || doc.Kind == 0 {
		return nil
	}
	if err := validateMappingKeys(doc, "suite root", map[string]bool{"suite": true, "candidates": true, "cases": true}); err != nil {
		return err
	}
	validateChild := func(key string, fn func(*yaml.Node) error) error {
		child := mappingValue(doc, key)
		if child == nil {
			return nil
		}
		return fn(child)
	}
	if err := validateChild("suite", func(node *yaml.Node) error {
		return validateMappingKeys(node, "suite", map[string]bool{"id": true, "name": true, "version": true})
	}); err != nil {
		return err
	}
	if err := validateChild("candidates", func(node *yaml.Node) error {
		if node.Kind != yaml.SequenceNode {
			return nil
		}
		for i, candidate := range node.Content {
			if err := validateMappingKeys(candidate, fmt.Sprintf("candidate[%d]", i), map[string]bool{
				"id": true, "profile": true, "stages": true,
				"max_agents": true, "max_concurrency": true,
			}); err != nil {
				return err
			}
			stages := mappingValue(candidate, "stages")
			if stages != nil {
				if err := validateMappingKeys(stages, fmt.Sprintf("candidate[%d] stages", i), map[string]bool{
					"selection": true, "reviewers": true, "synthesis": true,
				}); err != nil {
					return err
				}
			}
			selection := mappingValue(stages, "selection")
			if selection != nil {
				if err := validateMappingKeys(selection, fmt.Sprintf("candidate[%d] stages.selection", i), map[string]bool{
					"model": true, "effort": true, "prompt": true,
				}); err != nil {
					return err
				}
			}
			reviewers := mappingValue(stages, "reviewers")
			if reviewers != nil {
				if err := validateMappingKeys(reviewers, fmt.Sprintf("candidate[%d] stages.reviewers", i), map[string]bool{
					"model": true, "model_tier": true, "effort": true, "agent_dirs": true, "agents_dir": true,
				}); err != nil {
					return err
				}
			}
			synthesis := mappingValue(stages, "synthesis")
			if synthesis != nil {
				if err := validateMappingKeys(synthesis, fmt.Sprintf("candidate[%d] stages.synthesis", i), map[string]bool{
					"model": true, "effort": true, "prompt": true,
				}); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return validateChild("cases", func(node *yaml.Node) error {
		if node.Kind != yaml.SequenceNode {
			return nil
		}
		for i, benchCase := range node.Content {
			if err := validateMappingKeys(benchCase, fmt.Sprintf("case[%d]", i), map[string]bool{
				"id": true, "pr": true, "review_base_sha": true, "review_head_sha": true,
				"expected_base_sha": true, "expected_head_sha": true, "anchors": true,
			}); err != nil {
				return err
			}
			anchors := mappingValue(benchCase, "anchors")
			if anchors == nil || anchors.Kind != yaml.SequenceNode {
				continue
			}
			for j, anchor := range anchors.Content {
				if err := validateMappingKeys(anchor, fmt.Sprintf("case[%d] anchor[%d]", i, j), map[string]bool{
					"id": true, "file": true, "side": true, "lines": true,
				}); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func validateMappingKeys(node *yaml.Node, label string, allowed map[string]bool) error {
	// Non-mapping nodes are decoded and rejected by the typed YAML unmarshal;
	// this helper only enforces key allowlists when a mapping is present.
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if !allowed[key] {
			return fmt.Errorf("%w: %s unknown field %q", ErrInvalid, label, key)
		}
	}
	return nil
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func filterIDSet(label string, values []string) (map[string]bool, error) {
	out := map[string]bool{}
	for i, value := range values {
		trimmed := strings.TrimSpace(value)
		if err := validateID(fmt.Sprintf("%s filter[%d]", label, i), trimmed); err != nil {
			return nil, err
		}
		out[trimmed] = true
	}
	return out, nil
}

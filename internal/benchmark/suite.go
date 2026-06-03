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
	ID             string   `yaml:"id" json:"id"`
	Profile        string   `yaml:"profile" json:"profile"`
	Model          string   `yaml:"model,omitempty" json:"model,omitempty"`
	Effort         string   `yaml:"effort,omitempty" json:"effort,omitempty"`
	AgentDirs      []string `yaml:"agent_dirs,omitempty" json:"agent_dirs,omitempty"`
	MaxAgents      int      `yaml:"max_agents,omitempty" json:"max_agents,omitempty"`
	MaxConcurrency int      `yaml:"max_concurrency,omitempty" json:"max_concurrency,omitempty"`

	modelSet  bool
	effortSet bool
}

// UnmarshalYAML accepts the canonical agent_dirs field and the draft agents_dir
// alias, but rejects documents that use both on the same candidate.
func (c *Candidate) UnmarshalYAML(value *yaml.Node) error {
	type rawCandidate struct {
		ID             string   `yaml:"id"`
		Profile        string   `yaml:"profile"`
		Model          string   `yaml:"model"`
		Effort         string   `yaml:"effort"`
		AgentDirs      []string `yaml:"agent_dirs"`
		AgentsDir      []string `yaml:"agents_dir"`
		MaxAgents      int      `yaml:"max_agents"`
		MaxConcurrency int      `yaml:"max_concurrency"`
	}
	var raw rawCandidate
	if err := value.Decode(&raw); err != nil {
		return fmt.Errorf("%w: decode candidate: %w", ErrInvalid, err)
	}
	hasAgentDirs := mappingHasKey(value, "agent_dirs")
	hasAgentsDir := mappingHasKey(value, "agents_dir")
	if hasAgentDirs && hasAgentsDir {
		return fmt.Errorf("%w: candidate %q cannot set both agent_dirs and agents_dir", ErrInvalid, raw.ID)
	}
	agentDirs := raw.AgentDirs
	if hasAgentsDir {
		agentDirs = raw.AgentsDir
	}
	*c = Candidate{
		ID:             raw.ID,
		Profile:        raw.Profile,
		Model:          raw.Model,
		Effort:         raw.Effort,
		AgentDirs:      agentDirs,
		MaxAgents:      raw.MaxAgents,
		MaxConcurrency: raw.MaxConcurrency,
		modelSet:       mappingHasKey(value, "model"),
		effortSet:      mappingHasKey(value, "effort"),
	}
	return nil
}

// Case is one pull request to review during a benchmark.
type Case struct {
	ID              string   `yaml:"id" json:"id"`
	PR              string   `yaml:"pr" json:"pr"`
	ExpectedBaseSHA string   `yaml:"expected_base_sha,omitempty" json:"expected_base_sha,omitempty"`
	ExpectedHeadSHA string   `yaml:"expected_head_sha,omitempty" json:"expected_head_sha,omitempty"`
	Anchors         []Anchor `yaml:"anchors,omitempty" json:"anchors,omitempty"`

	expectedBaseSHASet bool
	expectedHeadSHASet bool
}

// UnmarshalYAML tracks optional SHA field presence so explicitly blank values
// can be rejected after normalization.
func (c *Case) UnmarshalYAML(value *yaml.Node) error {
	type rawCase struct {
		ID              string   `yaml:"id"`
		PR              string   `yaml:"pr"`
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
		ExpectedBaseSHA:    raw.ExpectedBaseSHA,
		ExpectedHeadSHA:    raw.ExpectedHeadSHA,
		Anchors:            raw.Anchors,
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
	suite.Suite.ID = strings.TrimSpace(suite.Suite.ID)
	suite.Suite.Name = strings.TrimSpace(suite.Suite.Name)
	for i := range suite.Candidates {
		c := &suite.Candidates[i]
		c.ID = strings.TrimSpace(c.ID)
		c.Profile = strings.TrimSpace(c.Profile)
		c.Model = strings.TrimSpace(c.Model)
		c.Effort = strings.TrimSpace(c.Effort)
		for j := range c.AgentDirs {
			c.AgentDirs[j] = strings.TrimSpace(c.AgentDirs[j])
		}
	}
	for i := range suite.Cases {
		c := &suite.Cases[i]
		c.ID = strings.TrimSpace(c.ID)
		c.PR = strings.TrimSpace(c.PR)
		c.ExpectedBaseSHA = strings.TrimSpace(c.ExpectedBaseSHA)
		c.ExpectedHeadSHA = strings.TrimSpace(c.ExpectedHeadSHA)
		for j := range c.Anchors {
			a := &c.Anchors[j]
			a.ID = strings.TrimSpace(a.ID)
			a.File = strings.TrimSpace(a.File)
			a.Side = strings.TrimSpace(a.Side)
		}
	}
}

// Validate checks suite schema and profile/case compatibility without running reviews.
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

// Select returns suite-order candidates and cases after optional ID filtering.
func Select(suite SuiteFile, candidateIDs, caseIDs []string) ([]Candidate, []Case, error) {
	candidateSet, err := filterIDSet("candidate", candidateIDs)
	if err != nil {
		return nil, nil, err
	}
	caseSet, err := filterIDSet("case", caseIDs)
	if err != nil {
		return nil, nil, err
	}
	selectedCandidates := make([]Candidate, 0, len(suite.Candidates))
	seenCandidates := map[string]bool{}
	for _, candidate := range suite.Candidates {
		if len(candidateSet) > 0 && !candidateSet[candidate.ID] {
			continue
		}
		selectedCandidates = append(selectedCandidates, candidate)
		seenCandidates[candidate.ID] = true
	}
	for id := range candidateSet {
		if !seenCandidates[id] {
			return nil, nil, fmt.Errorf("%w: unknown candidate %q", ErrInvalid, id)
		}
	}

	selectedCases := make([]Case, 0, len(suite.Cases))
	seenCases := map[string]bool{}
	for _, benchCase := range suite.Cases {
		if len(caseSet) > 0 && !caseSet[benchCase.ID] {
			continue
		}
		selectedCases = append(selectedCases, benchCase)
		seenCases[benchCase.ID] = true
	}
	for id := range caseSet {
		if !seenCases[id] {
			return nil, nil, fmt.Errorf("%w: unknown case %q", ErrInvalid, id)
		}
	}
	return selectedCandidates, selectedCases, nil
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
		if candidate.modelSet && candidate.Model == "" {
			return fmt.Errorf("%w: candidate %q model must be non-empty when present", ErrInvalid, candidate.ID)
		}
		if candidate.effortSet && candidate.Effort == "" {
			return fmt.Errorf("%w: candidate %q effort must be non-empty when present", ErrInvalid, candidate.ID)
		}
		if candidate.MaxAgents < 0 {
			return fmt.Errorf("%w: candidate %q max_agents must be non-negative", ErrInvalid, candidate.ID)
		}
		if candidate.MaxConcurrency < 0 {
			return fmt.Errorf("%w: candidate %q max_concurrency must be non-negative", ErrInvalid, candidate.ID)
		}
		for j, dir := range candidate.AgentDirs {
			if dir == "" {
				return fmt.Errorf("%w: candidate %q agent_dirs[%d] must be non-empty", ErrInvalid, candidate.ID, j)
			}
		}
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
		if _, err := prref.ParseGitHubPullURL(benchCase.PR); err != nil {
			return fmt.Errorf("%w: case %q invalid PR URL: %w", ErrInvalid, benchCase.ID, err)
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
	for _, candidate := range candidates {
		profile := cfg.Profiles[candidate.Profile]
		for _, benchCase := range cases {
			ref, err := prref.ParseGitHubPullURL(benchCase.PR)
			if err != nil {
				return fmt.Errorf("%w: case %q invalid PR URL: %w", ErrInvalid, benchCase.ID, err)
			}
			if !prref.SameHost(ref.Host, profile.Git.Host) {
				return fmt.Errorf("%w: candidate %q profile host %q does not match case %q PR host %q", ErrInvalid, candidate.ID, profile.Git.Host, benchCase.ID, ref.Host)
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
				"id": true, "profile": true, "model": true, "effort": true,
				"agent_dirs": true, "agents_dir": true,
				"max_agents": true, "max_concurrency": true,
			}); err != nil {
				return err
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
				"id": true, "pr": true, "expected_base_sha": true, "expected_head_sha": true, "anchors": true,
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
	if node.Kind != yaml.MappingNode {
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

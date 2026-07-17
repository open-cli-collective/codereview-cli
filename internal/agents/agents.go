// Package agents loads trusted review agent definitions.
package agents

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/gobwas/glob"
	"gopkg.in/yaml.v3"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/modelprefs"
	"github.com/open-cli-collective/codereview-cli/internal/prref"
)

const (
	repoAgentsRoot            = ".codereview/agents"
	maxGitWorktreeSearchDepth = 40
)

var (
	// ErrInvalid identifies malformed agent definitions or unsafe agent names.
	ErrInvalid = errors.New("agents: invalid")
	// ErrNotFound identifies a requested agent absent from a loaded catalog.
	ErrNotFound = errors.New("agents: not found")
	// ErrUnsafeSource identifies profile agent sources that are mutable or
	// ambiguous for PR review execution.
	ErrUnsafeSource = errors.New("agents: unsafe source")
)

// SourceKind identifies where an agent definition came from.
type SourceKind string

// Supported source kinds.
const (
	SourceProfile SourceKind = "profile"
	SourceRepo    SourceKind = "repo"
	SourceFlag    SourceKind = "flag"
)

// SourceStatus reports whether a configured source is usable.
type SourceStatus string

// Supported source statuses.
const (
	SourceStatusAvailable  SourceStatus = "available"
	SourceStatusMissing    SourceStatus = "missing"
	SourceStatusUnreadable SourceStatus = "unreadable"
	SourceStatusInvalid    SourceStatus = "invalid"
)

// SourceInfo describes one configured agent source for operator audit.
type SourceInfo struct {
	Kind            SourceKind   `json:"kind"`
	Label           string       `json:"label,omitempty"`
	Ref             string       `json:"ref,omitempty"`
	SHA             string       `json:"sha,omitempty"`
	Index           int          `json:"index,omitempty"`
	ProvenanceLabel string       `json:"provenance"`
	ConfiguredPath  string       `json:"configured_path,omitempty"`
	CanonicalPath   string       `json:"canonical_path,omitempty"`
	Present         bool         `json:"present"`
	Status          SourceStatus `json:"status"`
	Fingerprint     string       `json:"fingerprint,omitempty"`
	Warnings        []string     `json:"warnings,omitempty"`
	Error           string       `json:"error,omitempty"`
}

// Provenance identifies the winning source for one loaded agent.
type Provenance struct {
	Kind           SourceKind `json:"kind"`
	Label          string     `json:"label,omitempty"`
	Ref            string     `json:"ref,omitempty"`
	SHA            string     `json:"sha,omitempty"`
	Index          int        `json:"index,omitempty"`
	ConfiguredPath string     `json:"configured_path,omitempty"`
	CanonicalPath  string     `json:"canonical_path,omitempty"`
	Fingerprint    string     `json:"fingerprint,omitempty"`
	Warnings       []string   `json:"warnings,omitempty"`
}

// String returns the user-facing provenance label.
func (p Provenance) String() string {
	switch p.Kind {
	case SourceProfile:
		return "profile:" + p.Label
	case SourceRepo:
		if p.SHA != "" {
			return "repo@" + p.Ref + ":" + prref.ShortSHA(p.SHA)
		}
		return "repo@" + p.Ref
	case SourceFlag:
		return fmt.Sprintf("flag:%d", p.Index)
	default:
		return string(p.Kind)
	}
}

// SourceInfo returns the structured source details for this provenance.
func (p Provenance) SourceInfo() SourceInfo {
	info := SourceInfo{
		Kind:            p.Kind,
		Label:           p.Label,
		Ref:             p.Ref,
		SHA:             p.SHA,
		Index:           p.Index,
		ProvenanceLabel: p.String(),
		ConfiguredPath:  p.ConfiguredPath,
		CanonicalPath:   p.CanonicalPath,
		Present:         true,
		Status:          SourceStatusAvailable,
		Fingerprint:     p.Fingerprint,
		Warnings:        append([]string(nil), p.Warnings...),
	}
	return info
}

// Category is one agent category definition.
type Category struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Owner       string `json:"owner,omitempty"`
}

// Agent is one loaded review agent.
type Agent struct {
	ID                   string     `json:"id"`
	Name                 string     `json:"name"`
	Category             Category   `json:"category"`
	Description          string     `json:"description,omitempty"`
	ModelTier            string     `json:"model_tier,omitempty"`
	ModelID              string     `json:"model_id,omitempty"`
	Effort               string     `json:"effort,omitempty"`
	FileGlobs            []string   `json:"file_globs,omitempty"`
	AppliesWhen          []string   `json:"applies_when,omitempty"`
	RequiredOnMatch      bool       `json:"required_on_match,omitempty"`
	NeedsFullFileContent bool       `json:"needs_full_file_content"`
	Prompt               string     `json:"prompt,omitempty"`
	Provenance           Provenance `json:"provenance"`
	Overridden           []string   `json:"overridden,omitempty"`
}

// RepoReader is the narrow read seam needed to load repo-local agent files.
type RepoReader interface {
	ListTreeAtRef(ctx context.Context, ref gitprovider.PRRef, gitRef string, treePath string) ([]gitprovider.TreeEntry, error)
	GetFileAtRef(ctx context.Context, ref gitprovider.PRRef, gitRef string, filePath string) ([]byte, error)
}

// RepoSource pins repo-local agents to a resolved pull request base SHA.
type RepoSource struct {
	Reader RepoReader
	Ref    gitprovider.PRRef
	PR     gitprovider.PR
}

// LoadOptions configures catalog loading.
type LoadOptions struct {
	ProfileDirs               []string
	Repo                      *RepoSource
	FlagDirs                  []string
	RequireSafeProfileSources bool
	SafeProfileSourceRoot     string
	AllowSoftRepoFailures     bool
}

// RepoInfo describes the trusted repo-local source, when one was considered.
type RepoInfo struct {
	Ref        string `json:"ref"`
	SHA        string `json:"sha"`
	Provenance string `json:"provenance"`
}

// TrustNote returns the human-readable base-branch trust-boundary note.
func (r RepoInfo) TrustNote() string {
	if r.Ref == "" && r.SHA == "" {
		return ""
	}
	return fmt.Sprintf("Repo-local agents loaded from base branch %s at %s; PR-head .codereview/agents changes do not affect this listing.", r.Ref, prref.ShortSHA(r.SHA))
}

// Catalog is the merged, precedence-resolved agent catalog.
type Catalog struct {
	Agents  []Agent      `json:"agents"`
	Repo    *RepoInfo    `json:"repo,omitempty"`
	Sources []SourceInfo `json:"sources,omitempty"`
}

// Find returns one agent by full ID.
func (c Catalog) Find(id string) (Agent, bool) {
	for _, agent := range c.Agents {
		if agent.ID == id {
			return agent, true
		}
	}
	return Agent{}, false
}

// Load builds a merged catalog from all configured sources.
func Load(ctx context.Context, opts LoadOptions) (Catalog, error) {
	var merged catalogBuilder
	var sources []SourceInfo
	for _, dir := range opts.ProfileDirs {
		provenance := Provenance{Kind: SourceProfile}
		source, err := inspectFileSource(dir, provenance)
		if err != nil {
			return Catalog{}, err
		}
		if opts.RequireSafeProfileSources {
			if err := unsafeSourceError(source, opts.SafeProfileSourceRoot); err != nil {
				return Catalog{}, err
			}
		}
		agents, source, err := loadFileSourceFromSource(source)
		if err != nil {
			return Catalog{}, err
		}
		sources = append(sources, source)
		merged.add(agents)
	}

	var repoInfo *RepoInfo
	if opts.Repo != nil {
		provenance, err := repoProvenance(opts.Repo.PR)
		if err != nil {
			return Catalog{}, err
		}
		repoInfo = &RepoInfo{
			Ref:        provenance.Ref,
			SHA:        provenance.SHA,
			Provenance: provenance.String(),
		}
		agents, repoSource, err := loadRepoSource(ctx, *opts.Repo, provenance, opts.AllowSoftRepoFailures)
		if err != nil {
			return Catalog{}, err
		}
		sources = append(sources, repoSource)
		merged.add(agents)
	}

	for i, dir := range opts.FlagDirs {
		provenance := Provenance{Kind: SourceFlag, Index: i + 1}
		agents, source, err := loadFileSource(dir, provenance)
		if err != nil {
			return Catalog{}, err
		}
		sources = append(sources, source)
		merged.add(agents)
	}

	return Catalog{Agents: merged.sorted(), Repo: repoInfo, Sources: sources}, nil
}

// InspectProfileSources reports active-profile source status without failing on
// missing or unreadable deployment material.
func InspectProfileSources(dirs []string) []SourceInfo {
	out := make([]SourceInfo, 0, len(dirs))
	for _, dir := range dirs {
		source, _ := inspectFileSource(dir, Provenance{Kind: SourceProfile})
		out = append(out, source)
	}
	return out
}

// RequireSafeProfileSources fails when profile sources are mutable or
// ambiguous for PR review execution.
func RequireSafeProfileSources(dirs []string, invocationRoot string) error {
	for _, dir := range dirs {
		source, err := inspectFileSource(dir, Provenance{Kind: SourceProfile})
		if err != nil {
			return err
		}
		if err := unsafeSourceError(source, invocationRoot); err != nil {
			return err
		}
	}
	return nil
}

func unsafeSourceError(source SourceInfo, invocationRoot string) error {
	reasons := unsafeSourceReasons(source, invocationRoot)
	if len(reasons) == 0 {
		return nil
	}
	return fmt.Errorf("%w: profile agent source %s is not trusted for PR review: %s", ErrUnsafeSource, source.ConfiguredPath, strings.Join(reasons, "; "))
}

func unsafeSourceReasons(source SourceInfo, invocationRoot string) []string {
	expanded, err := expandPath(source.ConfiguredPath)
	reasons := sourcePathWarnings(expanded, source.CanonicalPath, err == nil)
	if pathWithin(source.CanonicalPath, invocationRoot) {
		root := invocationRoot
		if canonicalRoot, err := canonicalizePath(invocationRoot); err == nil {
			root = canonicalRoot
		}
		reasons = append(reasons, fmt.Sprintf("canonical path resolves inside the current invocation worktree %s; PR authors may be able to mutate it", root))
	}
	return reasons
}

type catalogBuilder struct {
	agents map[string]Agent
}

func (b *catalogBuilder) add(agents []Agent) {
	if b.agents == nil {
		b.agents = make(map[string]Agent)
	}
	for _, agent := range agents {
		if previous, ok := b.agents[agent.ID]; ok {
			agent.Overridden = append(previous.Overridden, previous.Provenance.String())
		}
		b.agents[agent.ID] = agent
	}
}

func (b *catalogBuilder) sorted() []Agent {
	if len(b.agents) == 0 {
		return nil
	}
	return slices.SortedFunc(maps.Values(b.agents), func(a, b Agent) int {
		return cmp.Compare(a.ID, b.ID)
	})
}

type categoryYAML struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Owner       string `yaml:"owner"`
}

type agentYAML struct {
	Name                 string   `yaml:"name"`
	Description          string   `yaml:"description"`
	ModelTier            string   `yaml:"model_tier"`
	ModelID              string   `yaml:"model_id"`
	Effort               string   `yaml:"effort"`
	FileGlobs            []string `yaml:"file_globs"`
	AppliesWhen          []string `yaml:"applies_when"`
	RequiredOnMatch      bool     `yaml:"required_on_match"`
	NeedsFullFileContent bool     `yaml:"needs_full_file_content"`
}

func loadFileSource(rawDir string, provenance Provenance) ([]Agent, SourceInfo, error) {
	source, err := inspectFileSource(rawDir, provenance)
	if err != nil {
		return nil, source, err
	}
	agents, source, err := loadFileSourceFromSource(source)
	return agents, source, err
}

func loadFileSourceFromSource(source SourceInfo) ([]Agent, SourceInfo, error) {
	provenance := provenanceFromSource(source)
	dir := source.CanonicalPath
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, source, fmt.Errorf("agents: read source %s: %w", source.ConfiguredPath, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	var agents []Agent
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		categoryName := entry.Name()
		if err := validateName("category", categoryName); err != nil {
			return nil, source, err
		}
		categoryPath := filepath.Join(dir, categoryName)
		category, err := readFileCategory(filepath.Join(categoryPath, "index.yaml"), categoryName)
		if err != nil {
			return nil, source, err
		}
		categoryAgents, err := readFileAgents(categoryPath, category, provenance)
		if err != nil {
			return nil, source, err
		}
		agents = append(agents, categoryAgents...)
	}
	return agents, source, nil
}

func readFileCategory(filePath, pathName string) (Category, error) {
	var index categoryYAML
	if err := decodeYAMLFile(filePath, &index); err != nil {
		return Category{}, err
	}
	if err := validateMatchingName("category", pathName, index.Name); err != nil {
		return Category{}, err
	}
	return Category{Name: pathName, Description: index.Description, Owner: index.Owner}, nil
}

func readFileAgents(categoryPath string, category Category, provenance Provenance) ([]Agent, error) {
	entries, err := os.ReadDir(categoryPath)
	if err != nil {
		return nil, fmt.Errorf("agents: read category %s: %w", category.Name, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	var agents []Agent
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		agentName := entry.Name()
		if err := validateName("agent", agentName); err != nil {
			return nil, err
		}
		agentPath := filepath.Join(categoryPath, agentName)
		agent, err := readFileAgent(agentPath, category, agentName, provenance)
		if err != nil {
			return nil, err
		}
		agents = append(agents, agent)
	}
	return agents, nil
}

func readFileAgent(agentPath string, category Category, pathName string, provenance Provenance) (Agent, error) {
	var index agentYAML
	if err := decodeYAMLFile(filepath.Join(agentPath, "index.yaml"), &index); err != nil {
		return Agent{}, err
	}
	if err := validateMatchingName("agent", pathName, index.Name); err != nil {
		return Agent{}, err
	}
	if err := validateAgentYAML(category.Name, pathName, index); err != nil {
		return Agent{}, err
	}
	prompt, err := os.ReadFile(filepath.Join(agentPath, "prompt.md")) // #nosec G304 -- path is from an agent source directory.
	if err != nil {
		return Agent{}, fmt.Errorf("agents: read prompt for %s:%s: %w", category.Name, pathName, err)
	}
	return newAgent(category, pathName, index, string(prompt), provenance), nil
}

func loadRepoSource(ctx context.Context, source RepoSource, provenance Provenance, allowSoftFailures bool) ([]Agent, SourceInfo, error) {
	repoSource := provenance.SourceInfo()
	fail := func(err error) ([]Agent, SourceInfo, error) {
		if classified, ok := classifyRepoCatalogError(repoSource, err); allowSoftFailures && ok {
			return nil, classified, nil
		}
		return nil, repoSource, err
	}
	if source.Reader == nil {
		return nil, repoSource, fmt.Errorf("%w: repo reader is required", ErrInvalid)
	}
	if err := source.Ref.Validate(); err != nil {
		return nil, repoSource, err
	}
	if source.PR.Ref != (gitprovider.PRRef{}) && source.PR.Ref != source.Ref {
		return nil, repoSource, fmt.Errorf("%w: PR ref %v does not match source ref %v", ErrInvalid, source.PR.Ref, source.Ref)
	}
	baseSHA := strings.TrimSpace(source.PR.Base.SHA)
	if baseSHA == "" {
		return nil, repoSource, fmt.Errorf("%w: PR base SHA is required", ErrInvalid)
	}

	rootEntries, err := source.Reader.ListTreeAtRef(ctx, source.Ref, baseSHA, repoAgentsRoot)
	if errors.Is(err, gitprovider.ErrNotFound) {
		repoSource.Present = false
		repoSource.Status = SourceStatusMissing
		return nil, repoSource, nil
	}
	if err != nil {
		return nil, repoSource, err
	}
	sortTreeEntries(rootEntries)

	var agents []Agent
	loadedAgent := false
	for _, entry := range rootEntries {
		if entry.Type != "tree" {
			continue
		}
		categoryName, ok := directEntryName(repoAgentsRoot, entry.Path)
		if !ok {
			continue
		}
		if err := validateName("category", categoryName); err != nil {
			return fail(err)
		}
		categoryPath := path.Join(repoAgentsRoot, categoryName)
		category, err := readRepoCategory(ctx, source.Reader, source.Ref, baseSHA, categoryPath, categoryName)
		if err != nil {
			return fail(err)
		}
		categoryAgents, err := readRepoAgents(ctx, source.Reader, source.Ref, baseSHA, categoryPath, category, provenance)
		if err != nil {
			return fail(err)
		}
		if len(categoryAgents) == 0 {
			return fail(fmt.Errorf("%w: repo source %s category %q contains no agents", ErrInvalid, repoAgentsRoot, categoryName))
		}
		loadedAgent = true
		agents = append(agents, categoryAgents...)
	}
	if !loadedAgent {
		return fail(fmt.Errorf("%w: repo source %s contains no agents", ErrInvalid, repoAgentsRoot))
	}
	return agents, repoSource, nil
}

func classifyRepoCatalogError(source SourceInfo, err error) (SourceInfo, bool) {
	switch {
	case errors.Is(err, ErrInvalid):
		return repoSourceError(source, SourceStatusInvalid, err), true
	case errors.Is(err, gitprovider.ErrNotFound):
		return repoSourceError(source, SourceStatusUnreadable, err), true
	default:
		return SourceInfo{}, false
	}
}

func repoSourceError(source SourceInfo, status SourceStatus, err error) SourceInfo {
	source.Present = true
	source.Status = status
	if err != nil {
		source.Error = err.Error()
	}
	return source
}

func readRepoCategory(ctx context.Context, reader RepoReader, ref gitprovider.PRRef, gitRef, categoryPath, pathName string) (Category, error) {
	var index categoryYAML
	if err := decodeRepoYAML(ctx, reader, ref, gitRef, path.Join(categoryPath, "index.yaml"), &index); err != nil {
		return Category{}, err
	}
	if err := validateMatchingName("category", pathName, index.Name); err != nil {
		return Category{}, err
	}
	return Category{Name: pathName, Description: index.Description, Owner: index.Owner}, nil
}

func readRepoAgents(ctx context.Context, reader RepoReader, ref gitprovider.PRRef, gitRef, categoryPath string, category Category, provenance Provenance) ([]Agent, error) {
	entries, err := reader.ListTreeAtRef(ctx, ref, gitRef, categoryPath)
	if err != nil {
		return nil, err
	}
	sortTreeEntries(entries)

	var agents []Agent
	for _, entry := range entries {
		if entry.Type != "tree" {
			continue
		}
		agentName, ok := directEntryName(categoryPath, entry.Path)
		if !ok {
			continue
		}
		if err := validateName("agent", agentName); err != nil {
			return nil, err
		}
		agentPath := path.Join(categoryPath, agentName)
		agent, err := readRepoAgent(ctx, reader, ref, gitRef, agentPath, category, agentName, provenance)
		if err != nil {
			return nil, err
		}
		agents = append(agents, agent)
	}
	return agents, nil
}

func readRepoAgent(ctx context.Context, reader RepoReader, ref gitprovider.PRRef, gitRef, agentPath string, category Category, pathName string, provenance Provenance) (Agent, error) {
	var index agentYAML
	if err := decodeRepoYAML(ctx, reader, ref, gitRef, path.Join(agentPath, "index.yaml"), &index); err != nil {
		return Agent{}, err
	}
	if err := validateMatchingName("agent", pathName, index.Name); err != nil {
		return Agent{}, err
	}
	if err := validateAgentYAML(category.Name, pathName, index); err != nil {
		return Agent{}, err
	}
	prompt, err := reader.GetFileAtRef(ctx, ref, gitRef, path.Join(agentPath, "prompt.md"))
	if err != nil {
		return Agent{}, err
	}
	return newAgent(category, pathName, index, string(prompt), provenance), nil
}

func newAgent(category Category, name string, index agentYAML, prompt string, provenance Provenance) Agent {
	return Agent{
		ID:                   category.Name + ":" + name,
		Name:                 name,
		Category:             category,
		Description:          index.Description,
		ModelTier:            strings.TrimSpace(index.ModelTier),
		ModelID:              strings.TrimSpace(index.ModelID),
		Effort:               strings.TrimSpace(index.Effort),
		FileGlobs:            append([]string(nil), index.FileGlobs...),
		AppliesWhen:          append([]string(nil), index.AppliesWhen...),
		RequiredOnMatch:      index.RequiredOnMatch,
		NeedsFullFileContent: index.NeedsFullFileContent,
		Prompt:               prompt,
		Provenance:           provenance,
	}
}

func validateAgentYAML(categoryName, agentName string, index agentYAML) error {
	modelTier := strings.TrimSpace(index.ModelTier)
	modelID := strings.TrimSpace(index.ModelID)
	switch {
	case modelTier == "" && modelID == "":
		return fmt.Errorf("%w: agent %s:%s requires model_tier or model_id", ErrInvalid, categoryName, agentName)
	case modelTier != "" && modelID != "":
		return fmt.Errorf("%w: agent %s:%s cannot set both model_tier and model_id", ErrInvalid, categoryName, agentName)
	case modelTier != "":
		if !validModelTier(modelTier) {
			return fmt.Errorf("%w: agent %s:%s model_tier %q is invalid", ErrInvalid, categoryName, agentName, modelTier)
		}
	}

	effort := strings.TrimSpace(index.Effort)
	if effort == "" {
		return fmt.Errorf("%w: agent %s:%s effort is required", ErrInvalid, categoryName, agentName)
	}
	if !modelprefs.Effort(effort).Valid() {
		return fmt.Errorf("%w: agent %s:%s effort %q is invalid", ErrInvalid, categoryName, agentName, effort)
	}
	if index.RequiredOnMatch {
		if len(index.FileGlobs) == 0 {
			return fmt.Errorf("%w: agent %s:%s required_on_match requires file_globs", ErrInvalid, categoryName, agentName)
		}
		for _, pattern := range index.FileGlobs {
			if _, err := glob.Compile(pattern, '/'); err != nil {
				return fmt.Errorf("%w: agent %s:%s file_glob %q is invalid: %v", ErrInvalid, categoryName, agentName, pattern, err)
			}
		}
	}
	return nil
}

func validModelTier(value string) bool {
	return slices.Contains([]string{"small", "medium", "large"}, value)
}

func decodeYAMLFile(filePath string, out any) error {
	data, err := os.ReadFile(filePath) // #nosec G304 -- path is from an agent source directory.
	if err != nil {
		return fmt.Errorf("agents: read %s: %w", filePath, err)
	}
	return decodeYAML(filePath, data, out)
}

func decodeRepoYAML(ctx context.Context, reader RepoReader, ref gitprovider.PRRef, gitRef, filePath string, out any) error {
	data, err := reader.GetFileAtRef(ctx, ref, gitRef, filePath)
	if err != nil {
		return err
	}
	return decodeYAML(filePath, data, out)
}

func decodeYAML(name string, data []byte, out any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%w: %s is empty", ErrInvalid, name)
		}
		return fmt.Errorf("%w: parse %s: %w", ErrInvalid, name, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: parse %s: %w", ErrInvalid, name, err)
	} else if err == nil {
		return fmt.Errorf("%w: parse %s: multiple YAML documents are not supported", ErrInvalid, name)
	}
	return nil
}

func validateMatchingName(kind, pathName, yamlName string) error {
	if err := validateName(kind, yamlName); err != nil {
		return err
	}
	if yamlName != pathName {
		return fmt.Errorf("%w: %s name %q does not match path %q", ErrInvalid, kind, yamlName, pathName)
	}
	return nil
}

func validateName(kind, value string) error {
	if value == "" {
		return fmt.Errorf("%w: %s name is required", ErrInvalid, kind)
	}
	if value == "." {
		return fmt.Errorf("%w: unsafe %s name %q", ErrInvalid, kind, value)
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("%w: %s name %q has surrounding whitespace", ErrInvalid, kind, value)
	}
	if strings.Contains(value, "/") || strings.Contains(value, "\\") || strings.Contains(value, "..") || strings.Contains(value, ":") {
		return fmt.Errorf("%w: unsafe %s name %q", ErrInvalid, kind, value)
	}
	return nil
}

func repoProvenance(pr gitprovider.PR) (Provenance, error) {
	ref := strings.TrimSpace(pr.Base.Ref)
	if ref == "" {
		ref = strings.TrimSpace(pr.Base.Name)
	}
	if ref == "" {
		return Provenance{}, fmt.Errorf("%w: PR base ref is required", ErrInvalid)
	}
	sha := strings.TrimSpace(pr.Base.SHA)
	if sha == "" {
		return Provenance{}, fmt.Errorf("%w: PR base SHA is required", ErrInvalid)
	}
	return Provenance{Kind: SourceRepo, Ref: ref, SHA: sha}, nil
}

func inspectFileSource(rawDir string, provenance Provenance) (SourceInfo, error) {
	rawDir = strings.TrimSpace(rawDir)
	source := SourceInfo{
		Kind:           provenance.Kind,
		Index:          provenance.Index,
		ConfiguredPath: rawDir,
		Status:         SourceStatusInvalid,
		Label:          sourceLabel(rawDir),
	}
	source.ProvenanceLabel = provenanceFromSource(source).String()
	if rawDir == "" {
		err := fmt.Errorf("%w: agent source path is required", ErrInvalid)
		source.Error = err.Error()
		return source, err
	}
	expanded, err := expandPath(rawDir)
	if err != nil {
		source.Error = err.Error()
		return source, err
	}
	if strings.TrimSpace(expanded) == "" {
		err := fmt.Errorf("%w: agent source path is required", ErrInvalid)
		source.Error = err.Error()
		return source, err
	}
	if _, err := os.ReadDir(expanded); err != nil {
		source.Status = SourceStatusUnreadable
		source.Present = true
		if errors.Is(err, os.ErrNotExist) {
			source.Status = SourceStatusMissing
			source.Present = false
		}
		loadErr := fmt.Errorf("agents: read source %s: %w", rawDir, err)
		source.Error = loadErr.Error()
		return source, loadErr
	}
	canonical, err := filepath.EvalSymlinks(expanded)
	if err != nil {
		return failSource(source, fmt.Errorf("agents: canonicalize source %s: %w", rawDir, err))
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return failSource(source, fmt.Errorf("agents: canonicalize source %s: %w", rawDir, err))
	}
	fingerprint, err := fingerprintFileSource(canonical)
	if err != nil {
		return failSource(source, fmt.Errorf("agents: fingerprint source %s: %w", rawDir, err))
	}
	source.Present = true
	source.Status = SourceStatusAvailable
	source.CanonicalPath = canonical
	source.Fingerprint = fingerprint
	source.Warnings = sourceWarnings(expanded, canonical)
	source.ProvenanceLabel = provenanceFromSource(source).String()
	return source, nil
}

func failSource(source SourceInfo, err error) (SourceInfo, error) {
	source.Status = SourceStatusUnreadable
	source.Present = true
	source.Error = err.Error()
	return source, err
}

func provenanceFromSource(source SourceInfo) Provenance {
	return Provenance{
		Kind:           source.Kind,
		Label:          source.Label,
		Ref:            source.Ref,
		SHA:            source.SHA,
		Index:          source.Index,
		ConfiguredPath: source.ConfiguredPath,
		CanonicalPath:  source.CanonicalPath,
		Fingerprint:    source.Fingerprint,
		Warnings:       append([]string(nil), source.Warnings...),
	}
}

func fingerprintFileSource(root string) (string, error) {
	// This is a best-effort snapshot for operator audit. Managed source
	// directories should not be mutated while a command is loading agents.
	var files []string
	categories, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	for _, category := range categories {
		if !category.IsDir() {
			continue
		}
		categoryName := category.Name()
		categoryPath := filepath.Join(root, categoryName)
		files = append(files, filepath.ToSlash(filepath.Join(categoryName, "index.yaml")))

		agentEntries, err := os.ReadDir(categoryPath)
		if err != nil {
			return "", err
		}
		for _, agent := range agentEntries {
			if !agent.IsDir() {
				continue
			}
			agentName := agent.Name()
			files = append(files,
				filepath.ToSlash(filepath.Join(categoryName, agentName, "index.yaml")),
				filepath.ToSlash(filepath.Join(categoryName, agentName, "prompt.md")),
			)
		}
	}
	// The final relative-path sort is the canonical ordering guarantee for
	// stable cross-platform fingerprints.
	sort.Strings(files)
	hash := sha256.New()
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))) // #nosec G304 -- path is from an inspected agent source.
		if err != nil {
			return "", err
		}
		if _, err := fmt.Fprintf(hash, "%s\x00%d\x00", rel, len(data)); err != nil {
			return "", err
		}
		if _, err := hash.Write(data); err != nil {
			return "", err
		}
		if _, err := hash.Write([]byte{0}); err != nil {
			return "", err
		}
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))[:32], nil
}

func sourceWarnings(expanded, canonical string) []string {
	warnings := sourcePathWarnings(expanded, canonical, true)
	if root, ok := gitWorktreeRoot(canonical); ok {
		warnings = append(warnings, fmt.Sprintf("canonical path is inside Git worktree %s; PR review only blocks sources inside the current invocation worktree", root))
	}
	return warnings
}

func sourcePathWarnings(expanded, canonical string, checkRelative bool) []string {
	var warnings []string
	if checkRelative && !filepath.IsAbs(expanded) {
		warnings = append(warnings, "configured path is relative; use an absolute org-managed path for rollout")
	}
	if pathWithin(canonical, os.TempDir()) {
		warnings = append(warnings, "canonical path is under the OS temp directory; temp locations are mutable")
	}
	return warnings
}

func canonicalizePath(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("path is required")
	}
	resolved, err := filepath.EvalSymlinks(raw)
	if err != nil {
		return "", err
	}
	return filepath.Abs(resolved)
}

func pathWithin(child, parent string) bool {
	if strings.TrimSpace(child) == "" || strings.TrimSpace(parent) == "" {
		return false
	}
	childAbs, err := filepath.Abs(child)
	if err != nil {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(childAbs); err == nil {
		childAbs = resolved
	}
	parentAbs, err := filepath.Abs(parent)
	if err != nil {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(parentAbs); err == nil {
		parentAbs = resolved
	}
	rel, err := filepath.Rel(parentAbs, childAbs)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func gitWorktreeRoot(canonical string) (string, bool) {
	dir := canonical
	for depth := 0; depth < maxGitWorktreeSearchDepth; depth++ {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
	return "", false
}

func sourceLabel(rawDir string) string {
	if strings.TrimSpace(rawDir) == "" {
		return ""
	}
	dir, err := expandPath(rawDir)
	if err != nil {
		return rawDir
	}
	base := filepath.Base(filepath.Clean(dir))
	if base == "." || base == string(filepath.Separator) || base == "" {
		return dir
	}
	return base
}

func expandPath(raw string) (string, error) {
	if raw == "~" || strings.HasPrefix(raw, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("agents: resolve home directory: %w", err)
		}
		if raw == "~" {
			return home, nil
		}
		return filepath.Join(home, strings.TrimPrefix(raw, "~/")), nil
	}
	return raw, nil
}

func directEntryName(parent, entryPath string) (string, bool) {
	name := strings.Trim(entryPath, "/")
	parent = strings.Trim(parent, "/")
	if parent != "" {
		prefix := parent + "/"
		name = strings.TrimPrefix(name, prefix)
	}
	if name == "" || strings.Contains(name, "/") {
		return "", false
	}
	return name, true
}

func sortTreeEntries(entries []gitprovider.TreeEntry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
}

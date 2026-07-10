package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/agents"
	"github.com/open-cli-collective/codereview-cli/internal/fsatomic"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

func writeArtifacts(paths ArtifactPaths, rawDiff string, patches []FilePatch, catalog agents.Catalog, selection llm.Selection, findings []review.Finding, rollup string, reviewerRuntime map[string]reviewerRuntimeResolution) error {
	if err := os.MkdirAll(paths.Dir, 0o700); err != nil {
		return fmt.Errorf("pipeline: create artifact dir: %w", err)
	}
	if err := os.MkdirAll(paths.SlicesDir, 0o700); err != nil {
		return fmt.Errorf("pipeline: create slices dir: %w", err)
	}
	if err := fsatomic.WriteFileAtomic(paths.DiffPatch, []byte(rawDiff), 0o600); err != nil {
		return fmt.Errorf("pipeline: write diff: %w", err)
	}
	sourceJSON, err := json.MarshalIndent(agentSourcesArtifactFromCatalog(catalog, reviewerRuntime), "", "  ")
	if err != nil {
		return err
	}
	if err := fsatomic.WriteFileAtomic(paths.AgentSourcesJSON, append(sourceJSON, '\n'), 0o600); err != nil {
		return fmt.Errorf("pipeline: write agent source provenance: %w", err)
	}
	for _, selected := range selection.SelectedAgents {
		for _, file := range selected.Files {
			patch, ok := findPatch(patches, file)
			if !ok {
				continue
			}
			path, err := paths.SlicePatch(selected.AgentID, file)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return fmt.Errorf("pipeline: create slice dir: %w", err)
			}
			if err := fsatomic.WriteFileAtomic(path, []byte(patch.Patch), 0o600); err != nil {
				return fmt.Errorf("pipeline: write slice: %w", err)
			}
		}
	}
	findingsJSON, err := json.MarshalIndent(findings, "", "  ")
	if err != nil {
		return err
	}
	if err := fsatomic.WriteFileAtomic(paths.FindingsJSON, append(findingsJSON, '\n'), 0o600); err != nil {
		return fmt.Errorf("pipeline: write findings: %w", err)
	}
	if err := fsatomic.WriteFileAtomic(paths.RollupMarkdown, []byte(rollup+"\n"), 0o600); err != nil {
		return fmt.Errorf("pipeline: write rollup: %w", err)
	}
	return nil
}

type workbenchPreparationRequest struct {
	PRRef        gitprovider.PRRef
	ReviewPR     gitprovider.PR
	ChangedFiles []string
	Artifacts    ArtifactPaths
}

type workbenchMetadataArtifact struct {
	SchemaVersion     int                        `json:"schema_version"`
	SourceRepoRoot    string                     `json:"source_repo_root"`
	CheckoutMode      string                     `json:"checkout_mode"`
	PR                workbenchPRIdentity        `json:"pr"`
	Base              workbenchBranchArtifact    `json:"base"`
	Head              workbenchBranchArtifact    `json:"head"`
	RepoPath          string                     `json:"repo_path"`
	ScratchPath       string                     `json:"scratch_path"`
	ChangedFiles      []string                   `json:"changed_files,omitempty"`
	FingerprintInputs workbenchFingerprintInputs `json:"fingerprint_inputs"`
}

type workbenchPRIdentity struct {
	Host   string `json:"host"`
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}

type workbenchBranchArtifact struct {
	Host  string `json:"host,omitempty"`
	Owner string `json:"owner,omitempty"`
	Repo  string `json:"repo,omitempty"`
	Name  string `json:"name,omitempty"`
	Ref   string `json:"ref,omitempty"`
	SHA   string `json:"sha"`
}

type workbenchFingerprintInputs struct {
	PR             workbenchPRIdentity `json:"pr"`
	BaseSHA        string              `json:"base_sha"`
	HeadSHA        string              `json:"head_sha"`
	CheckoutMode   string              `json:"checkout_mode"`
	ChangedFiles   []string            `json:"changed_files,omitempty"`
	SourceRepoRoot string              `json:"source_repo_root"`
}

func prepareWorkbenchArtifacts(ctx context.Context, opts Options, req workbenchPreparationRequest) error {
	sourceRepoRoot, err := opts.resolveRepoRoot(ctx)
	if err != nil {
		return fmt.Errorf("pipeline: resolve source repo root: %w", err)
	}
	sourceRepoRoot = filepath.Clean(sourceRepoRoot)

	if err := os.RemoveAll(req.Artifacts.WorkbenchDir); err != nil {
		return fmt.Errorf("pipeline: reset workbench dir: %w", err)
	}
	for _, dir := range []string{req.Artifacts.WorkbenchDir, req.Artifacts.WorkbenchScratch} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("pipeline: create workbench dir: %w", err)
		}
	}

	baseRemoteURL, err := resolveWorkbenchBaseRemoteURL(ctx, opts, sourceRepoRoot, req.ReviewPR.Base)
	if err != nil {
		return err
	}
	if _, err := opts.gitCommand(ctx, "", "clone", "--no-checkout", "--no-hardlinks", sourceRepoRoot, req.Artifacts.WorkbenchRepoDir); err != nil {
		return fmt.Errorf("pipeline: clone workbench repo: %w", err)
	}
	remoteMatchesBaseHost, err := workbenchRemoteMatchesBaseHost(baseRemoteURL, req.ReviewPR.Base)
	if err != nil {
		return err
	}
	if !remoteMatchesBaseHost {
		return fmt.Errorf("pipeline: source repo origin %q does not match PR base repo %s/%s on %s", baseRemoteURL, req.ReviewPR.Base.Owner, req.ReviewPR.Base.Repo, req.ReviewPR.Base.Host)
	}

	if err := ensureWorkbenchCommit(ctx, opts, req.Artifacts.WorkbenchRepoDir, req.ReviewPR.Base, baseRemoteURL); err != nil {
		return err
	}
	headRemoteURL := baseRemoteURL
	if !sameBranchRepo(req.ReviewPR.Base, req.ReviewPR.Head) {
		headRemoteURL, err = deriveWorkbenchRemoteURL(baseRemoteURL, req.ReviewPR.Head)
		if err != nil {
			return fmt.Errorf("pipeline: derive head remote URL: %w", err)
		}
	}
	if err := ensureWorkbenchCommit(ctx, opts, req.Artifacts.WorkbenchRepoDir, req.ReviewPR.Head, headRemoteURL); err != nil {
		return err
	}
	if _, err := opts.gitCommand(ctx, req.Artifacts.WorkbenchRepoDir, "checkout", "--detach", req.ReviewPR.Head.SHA); err != nil {
		return fmt.Errorf("pipeline: checkout workbench head %s: %w", shortSHA(req.ReviewPR.Head.SHA), err)
	}
	if err := verifyWorkbenchClean(ctx, opts, req.Artifacts.WorkbenchRepoDir, req.ReviewPR.Head.SHA); err != nil {
		return err
	}

	changedFiles := append([]string(nil), req.ChangedFiles...)
	sort.Strings(changedFiles)
	prIdentity := workbenchPRIdentity{
		Host:   req.PRRef.Host,
		Owner:  req.PRRef.Owner,
		Repo:   req.PRRef.Repo,
		Number: req.PRRef.Number,
	}
	meta := workbenchMetadataArtifact{
		SchemaVersion:  workbenchMetadataSchemaVersion,
		SourceRepoRoot: sourceRepoRoot,
		CheckoutMode:   workbenchCheckoutModeArtifactClone,
		PR:             prIdentity,
		Base:           workbenchBranchArtifactFromRef(req.ReviewPR.Base),
		Head:           workbenchBranchArtifactFromRef(req.ReviewPR.Head),
		RepoPath:       req.Artifacts.WorkbenchRepoDir,
		ScratchPath:    req.Artifacts.WorkbenchScratch,
		ChangedFiles:   changedFiles,
		FingerprintInputs: workbenchFingerprintInputs{
			PR:             prIdentity,
			BaseSHA:        req.ReviewPR.Base.SHA,
			HeadSHA:        req.ReviewPR.Head.SHA,
			CheckoutMode:   workbenchCheckoutModeArtifactClone,
			ChangedFiles:   changedFiles,
			SourceRepoRoot: sourceRepoRoot,
		},
	}
	return writeJSONFile(req.Artifacts.WorkbenchMetadataPath(), meta)
}

func resolveWorkbenchBaseRemoteURL(ctx context.Context, opts Options, sourceRepoRoot string, base gitprovider.PRBranchRef) (string, error) {
	originOutput, err := opts.gitCommand(ctx, sourceRepoRoot, "remote", "get-url", "origin")
	if err != nil {
		return "", fmt.Errorf("pipeline: resolve source repo origin: %w", err)
	}
	originURL := strings.TrimSpace(string(originOutput))
	if originURL == "" {
		return "", fmt.Errorf("pipeline: source repo origin URL is empty")
	}
	host, owner, repo, _, err := parseWorkbenchRemoteURL(originURL)
	if err != nil {
		return "", fmt.Errorf("pipeline: parse source repo origin URL %q: %w", originURL, err)
	}
	if owner != base.Owner || repo != base.Repo {
		return "", fmt.Errorf("pipeline: source repo origin %q does not match PR base repo %s/%s", originURL, base.Owner, base.Repo)
	}
	if host == "" {
		return "", fmt.Errorf("pipeline: source repo origin %q did not include a host", originURL)
	}
	return originURL, nil
}

func workbenchRemoteMatchesBaseHost(remoteURL string, base gitprovider.PRBranchRef) (bool, error) {
	host, _, _, _, err := parseWorkbenchRemoteURL(remoteURL)
	if err != nil {
		return false, fmt.Errorf("pipeline: parse source repo origin URL %q: %w", remoteURL, err)
	}
	return strings.EqualFold(host, base.Host), nil
}

func ensureWorkbenchCommit(ctx context.Context, opts Options, repoDir string, branch gitprovider.PRBranchRef, remoteURL string) error {
	ref := strings.TrimSpace(branch.Ref)
	if err := validateWorkbenchFetchRef(ref); err != nil {
		return err
	}
	if workbenchCommitPresent(ctx, opts, repoDir, branch.SHA) {
		return nil
	}
	if _, err := opts.gitCommand(ctx, repoDir, "fetch", "--no-tags", remoteURL, branch.SHA); err == nil && workbenchCommitPresent(ctx, opts, repoDir, branch.SHA) {
		return nil
	}
	if ref != "" {
		if _, err := opts.gitCommand(ctx, repoDir, "fetch", "--no-tags", remoteURL, ref); err == nil && workbenchCommitPresent(ctx, opts, repoDir, branch.SHA) {
			return nil
		}
	}
	return fmt.Errorf("pipeline: fetch commit %s for %s/%s from %q", shortSHA(branch.SHA), branch.Owner, branch.Repo, remoteURL)
}

func validateWorkbenchFetchRef(ref string) error {
	if ref == "" {
		return nil
	}
	if !strings.HasPrefix(ref, "refs/") {
		return fmt.Errorf("pipeline: reject unsafe fetch ref %q", ref)
	}
	return nil
}

func workbenchCommitPresent(ctx context.Context, opts Options, repoDir, sha string) bool {
	if strings.TrimSpace(sha) == "" {
		return false
	}
	_, err := opts.gitCommand(ctx, repoDir, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

func sameBranchRepo(left, right gitprovider.PRBranchRef) bool {
	return strings.EqualFold(left.Host, right.Host) && left.Owner == right.Owner && left.Repo == right.Repo
}

func workbenchBranchArtifactFromRef(ref gitprovider.PRBranchRef) workbenchBranchArtifact {
	return workbenchBranchArtifact{
		Host:  ref.Host,
		Owner: ref.Owner,
		Repo:  ref.Repo,
		Name:  ref.Name,
		Ref:   ref.Ref,
		SHA:   ref.SHA,
	}
}

func verifyWorkbenchClean(ctx context.Context, opts Options, repoDir string, headSHA string) error {
	head, err := opts.gitCommand(ctx, repoDir, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("pipeline: verify workbench head: %w", err)
	}
	if got := strings.TrimSpace(string(head)); got != strings.TrimSpace(headSHA) {
		return fmt.Errorf("pipeline: workbench head %s does not match expected %s", shortSHA(got), shortSHA(headSHA))
	}
	status, err := opts.gitCommand(ctx, repoDir, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("pipeline: verify workbench status: %w", err)
	}
	if strings.TrimSpace(string(status)) != "" {
		return fmt.Errorf("pipeline: workbench has local changes")
	}
	return nil
}

func buildReviewerWorkspaceRequest(ctx context.Context, opts Options, artifacts ArtifactPaths, headSHA string, agentID string, allowedFiles []string, model, effort, prompt, logPath string) (llm.Request, func() error, error) {
	if err := llm.RequireReviewerWorkspace(opts.Adapter); err != nil {
		return llm.Request{}, nil, fmt.Errorf("pipeline: %w", err)
	}
	workspace, cleanup, err := prepareReviewerWorkspace(ctx, opts, artifacts, headSHA, agentID, allowedFiles, defaultReviewerWorkspaceToolOutputBytes)
	if err != nil {
		return llm.Request{}, nil, err
	}
	currentCleanup := cleanup
	cleanupCurrent := func() error {
		if currentCleanup == nil {
			return nil
		}
		cleanupErr := currentCleanup()
		currentCleanup = nil
		return cleanupErr
	}
	return llm.Request{
		Model:             model,
		Effort:            effort,
		Prompt:            prompt,
		LogPath:           logPath,
		ReviewerWorkspace: &workspace,
		OnValidationRetry: func(req *llm.Request) error {
			if err := cleanupCurrent(); err != nil {
				return fmt.Errorf("pipeline: cleanup reviewer workspace before retry: %w", err)
			}
			retryWorkspace, retryCleanup, err := prepareReviewerWorkspace(ctx, opts, artifacts, headSHA, agentID, allowedFiles, defaultReviewerWorkspaceToolOutputBytes)
			if err != nil {
				return err
			}
			currentCleanup = retryCleanup
			req.ReviewerWorkspace = &retryWorkspace
			req.FreshValidationRetrySession = true
			return nil
		},
	}, cleanupCurrent, nil
}

func prepareReviewerWorkspace(ctx context.Context, opts Options, artifacts ArtifactPaths, headSHA string, agentID string, allowedFiles []string, maxToolOutputBytes int) (llm.ReviewerWorkspaceRequest, func() error, error) {
	if strings.TrimSpace(artifacts.WorkbenchRepoDir) == "" {
		return llm.ReviewerWorkspaceRequest{}, nil, fmt.Errorf("pipeline: workbench repo dir is required for reviewer workspace")
	}
	if strings.TrimSpace(artifacts.WorkbenchScratch) == "" {
		return llm.ReviewerWorkspaceRequest{}, nil, fmt.Errorf("pipeline: workbench scratch dir is required for reviewer workspace")
	}
	if strings.TrimSpace(agentID) == "" {
		return llm.ReviewerWorkspaceRequest{}, nil, fmt.Errorf("pipeline: agent ID is required for reviewer workspace")
	}
	encodedAgentID := statepaths.Encode(agentID)
	workspaceRoot := filepath.Join(artifacts.WorkbenchDir, "reviewers", encodedAgentID)
	workspaceRepo := filepath.Join(workspaceRoot, "repo")
	workspaceScratch := filepath.Join(artifacts.WorkbenchScratch, encodedAgentID)
	for _, dir := range []string{workspaceRoot, workspaceScratch} {
		if err := os.RemoveAll(dir); err != nil {
			return llm.ReviewerWorkspaceRequest{}, nil, fmt.Errorf("pipeline: reset reviewer workspace: %w", err)
		}
	}
	for _, dir := range []string{workspaceRoot, workspaceScratch} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return llm.ReviewerWorkspaceRequest{}, nil, fmt.Errorf("pipeline: create reviewer workspace: %w", err)
		}
	}
	cleanup := func() error {
		var cleanupErr error
		for _, dir := range []string{workspaceRoot, workspaceScratch} {
			if err := os.RemoveAll(dir); err != nil && cleanupErr == nil {
				cleanupErr = err
			}
		}
		return cleanupErr
	}
	if _, err := opts.gitCommand(ctx, "", "clone", "--no-hardlinks", artifacts.WorkbenchRepoDir, workspaceRepo); err != nil {
		_ = cleanup()
		return llm.ReviewerWorkspaceRequest{}, nil, fmt.Errorf("pipeline: clone reviewer workspace: %w", err)
	}
	if _, err := opts.gitCommand(ctx, workspaceRepo, "checkout", "--detach", headSHA); err != nil {
		_ = cleanup()
		return llm.ReviewerWorkspaceRequest{}, nil, fmt.Errorf("pipeline: checkout reviewer workspace head %s: %w", shortSHA(headSHA), err)
	}
	if err := verifyWorkbenchClean(ctx, opts, workspaceRepo, headSHA); err != nil {
		_ = cleanup()
		return llm.ReviewerWorkspaceRequest{}, nil, err
	}
	if len(allowedFiles) > 0 {
		for _, path := range allowedFiles {
			clean := filepath.Clean(strings.TrimSpace(path))
			if isReviewerWorkspaceEscapePath(clean) {
				_ = cleanup()
				return llm.ReviewerWorkspaceRequest{}, nil, fmt.Errorf("pipeline: invalid reviewer workspace file %q", path)
			}
			if err := validateReviewerWorkspaceFileTarget(filepath.Join(workspaceRepo, clean), clean); err != nil {
				_ = cleanup()
				return llm.ReviewerWorkspaceRequest{}, nil, err
			}
		}
	}
	tempDir := filepath.Join(workspaceScratch, "tmp")
	cacheDir := filepath.Join(workspaceScratch, "cache")
	goCacheDir := filepath.Join(cacheDir, "go-build")
	goTmpDir := filepath.Join(tempDir, "go")
	xdgCacheDir := filepath.Join(cacheDir, "xdg")
	for _, dir := range []string{tempDir, cacheDir, goCacheDir, goTmpDir, xdgCacheDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			_ = cleanup()
			return llm.ReviewerWorkspaceRequest{}, nil, fmt.Errorf("pipeline: create reviewer workspace support dir: %w", err)
		}
	}
	return llm.ReviewerWorkspaceRequest{
		RepoDir:            workspaceRepo,
		ScratchDir:         workspaceScratch,
		TempDir:            tempDir,
		CacheDir:           cacheDir,
		Env:                reviewerWorkspaceEnv(tempDir, goCacheDir, goTmpDir, xdgCacheDir),
		AllowedFiles:       append([]string(nil), allowedFiles...),
		MaxToolOutputBytes: maxToolOutputBytes,
	}, cleanup, nil
}

func reviewerWorkspaceEnv(tempDir, goCacheDir, goTmpDir, xdgCacheDir string) []string {
	return []string{
		"TMPDIR=" + tempDir,
		"TMP=" + tempDir,
		"TEMP=" + tempDir,
		"GOCACHE=" + goCacheDir,
		"GOTMPDIR=" + goTmpDir,
		"XDG_CACHE_HOME=" + xdgCacheDir,
	}
}

func isReviewerWorkspaceEscapePath(clean string) bool {
	return clean == "." || clean == "" || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator))
}

func validateReviewerWorkspaceFileTarget(target string, displayPath string) error {
	info, err := os.Lstat(target) // #nosec G304 -- target is derived from the pipeline-owned workbench checkout root plus validated relative paths.
	if err != nil {
		return fmt.Errorf("pipeline: stat reviewer workspace file %s: %w", displayPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("pipeline: reviewer workspace file %s must not be a symlink", displayPath)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("pipeline: reviewer workspace file %s must be a regular file", displayPath)
	}
	return nil
}

type workbenchRemoteStyle struct {
	scheme string
	user   string
	host   string
	scp    bool
	dotGit bool
}

func parseWorkbenchRemoteURL(raw string) (host, owner, repo string, style workbenchRemoteStyle, err error) {
	raw = strings.TrimSpace(raw)
	switch {
	case strings.Contains(raw, "://"):
		parsed, parseErr := url.Parse(raw)
		if parseErr != nil {
			return "", "", "", workbenchRemoteStyle{}, parseErr
		}
		owner, repo, dotGit, parseErr := parseWorkbenchRepoPath(parsed.Path)
		if parseErr != nil {
			return "", "", "", workbenchRemoteStyle{}, parseErr
		}
		style = workbenchRemoteStyle{
			scheme: parsed.Scheme,
			host:   parsed.Host,
			dotGit: dotGit,
		}
		if parsed.User != nil {
			style.user = parsed.User.Username()
		}
		return parsed.Host, owner, repo, style, nil
	case strings.Contains(raw, "@") && strings.Contains(raw, ":"):
		parts := strings.SplitN(raw, ":", 2)
		if len(parts) != 2 {
			return "", "", "", workbenchRemoteStyle{}, fmt.Errorf("invalid scp-style remote")
		}
		userHost := parts[0]
		pathPart := parts[1]
		userHostParts := strings.SplitN(userHost, "@", 2)
		if len(userHostParts) != 2 {
			return "", "", "", workbenchRemoteStyle{}, fmt.Errorf("invalid scp-style remote")
		}
		owner, repo, dotGit, parseErr := parseWorkbenchRepoPath(pathPart)
		if parseErr != nil {
			return "", "", "", workbenchRemoteStyle{}, parseErr
		}
		style = workbenchRemoteStyle{
			user:   userHostParts[0],
			host:   userHostParts[1],
			scp:    true,
			dotGit: dotGit,
		}
		return userHostParts[1], owner, repo, style, nil
	default:
		return "", "", "", workbenchRemoteStyle{}, fmt.Errorf("unsupported remote URL %q", raw)
	}
}

func parseWorkbenchRepoPath(path string) (owner, repo string, dotGit bool, err error) {
	path = strings.Trim(strings.TrimSpace(path), "/")
	if strings.HasSuffix(path, ".git") {
		dotGit = true
		path = strings.TrimSuffix(path, ".git")
	}
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return "", "", false, fmt.Errorf("unsupported repo path %q", path)
	}
	return parts[len(parts)-2], parts[len(parts)-1], dotGit, nil
}

func deriveWorkbenchRemoteURL(originURL string, branch gitprovider.PRBranchRef) (string, error) {
	_, _, _, style, err := parseWorkbenchRemoteURL(originURL)
	if err != nil {
		return "", err
	}
	repoPath := branch.Owner + "/" + branch.Repo
	if style.dotGit {
		repoPath += ".git"
	}
	if style.scp {
		return style.user + "@" + branch.Host + ":" + repoPath, nil
	}
	u := &url.URL{
		Scheme: style.scheme,
		Host:   branch.Host,
		Path:   "/" + repoPath,
	}
	if style.user != "" {
		u.User = url.User(style.user)
	}
	return u.String(), nil
}

func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func agentSourcesArtifactFromCatalog(catalog agents.Catalog, reviewerRuntime map[string]reviewerRuntimeResolution) agentSourcesArtifact {
	artifact := agentSourcesArtifact{
		Sources: append([]agents.SourceInfo(nil), catalog.Sources...),
		Agents:  make([]agentProvenanceArtifact, 0, len(catalog.Agents)),
	}
	for i := range artifact.Sources {
		artifact.Sources[i].Warnings = append([]string(nil), catalog.Sources[i].Warnings...)
	}
	for _, agent := range catalog.Agents {
		runtime, ok := reviewerRuntime[agent.ID]
		var runtimePtr *reviewerRuntimeResolution
		if ok {
			runtimeCopy := runtime
			runtimePtr = &runtimeCopy
		}
		artifact.Agents = append(artifact.Agents, agentProvenanceArtifact{
			ID:              agent.ID,
			Provenance:      agent.Provenance.String(),
			Source:          agent.Provenance.SourceInfo(),
			ReviewerRuntime: runtimePtr,
		})
	}
	return artifact
}

func reviewerRuntimeArtifact(req Request, catalog agents.Catalog, selection llm.Selection) map[string]reviewerRuntimeResolution {
	if strings.TrimSpace(req.ReviewerModelOverride) != "" {
		return nil
	}
	if len(selection.SelectedAgents) == 0 {
		return nil
	}
	agentsByID := make(map[string]agents.Agent, len(catalog.Agents))
	for _, agent := range catalog.Agents {
		agentsByID[agent.ID] = agent
	}
	out := make(map[string]reviewerRuntimeResolution, len(selection.SelectedAgents))
	for _, selected := range selection.SelectedAgents {
		agent, ok := agentsByID[selected.AgentID]
		if !ok {
			continue
		}
		resolution, err := resolveAgentModel(req.Profile, req.ReviewerModelTierOverride, agent)
		if err != nil {
			continue
		}
		out[selected.AgentID] = resolution
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Package workbench prepares isolated repository workspaces for reviews.
package workbench

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/fsatomic"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/prref"
	"github.com/open-cli-collective/codereview-cli/internal/reporoot"
	"github.com/open-cli-collective/codereview-cli/internal/runartifact"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

const (
	metadataSchemaVersion                   = 1
	checkoutModeArtifactClone               = "artifact-clone"
	defaultReviewerWorkspaceToolOutputBytes = 32 * 1024
)

// Deps contains the injected repository operations used to prepare workspaces.
type Deps struct {
	GitCommand      func(context.Context, string, ...string) ([]byte, error)
	ResolveRepoRoot func(context.Context) (string, error)
}

// Request identifies the review checkout and artifact paths to prepare.
type Request struct {
	PRRef        gitprovider.PRRef
	ReviewPR     gitprovider.PR
	ChangedFiles []string
	Artifacts    runartifact.Paths
}

type metadataArtifact struct {
	SchemaVersion     int               `json:"schema_version"`
	SourceRepoRoot    string            `json:"source_repo_root"`
	CheckoutMode      string            `json:"checkout_mode"`
	PR                prIdentity        `json:"pr"`
	Base              branchArtifact    `json:"base"`
	Head              branchArtifact    `json:"head"`
	RepoPath          string            `json:"repo_path"`
	ScratchPath       string            `json:"scratch_path"`
	ChangedFiles      []string          `json:"changed_files,omitempty"`
	FingerprintInputs fingerprintInputs `json:"fingerprint_inputs"`
}

type prIdentity struct {
	Host   string `json:"host"`
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}

type branchArtifact struct {
	Host  string `json:"host,omitempty"`
	Owner string `json:"owner,omitempty"`
	Repo  string `json:"repo,omitempty"`
	Name  string `json:"name,omitempty"`
	Ref   string `json:"ref,omitempty"`
	SHA   string `json:"sha"`
}

type fingerprintInputs struct {
	PR             prIdentity `json:"pr"`
	BaseSHA        string     `json:"base_sha"`
	HeadSHA        string     `json:"head_sha"`
	CheckoutMode   string     `json:"checkout_mode"`
	ChangedFiles   []string   `json:"changed_files,omitempty"`
	SourceRepoRoot string     `json:"source_repo_root"`
}

// Prepare creates a clean checkout pinned to the requested review commits.
func Prepare(ctx context.Context, deps Deps, req Request) error {
	sourceRepoRoot, err := deps.resolveRepoRoot(ctx)
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

	baseRemoteURL, err := resolveBaseRemoteURL(ctx, deps, sourceRepoRoot, req.ReviewPR.Base)
	if err != nil {
		return err
	}
	if _, err := deps.gitCommand(ctx, "", "clone", "--no-checkout", "--no-hardlinks", sourceRepoRoot, req.Artifacts.WorkbenchRepoDir); err != nil {
		return fmt.Errorf("pipeline: clone workbench repo: %w", err)
	}
	remoteMatchesBaseHost, err := remoteMatchesBaseHost(baseRemoteURL, req.ReviewPR.Base)
	if err != nil {
		return err
	}
	if !remoteMatchesBaseHost {
		return fmt.Errorf("pipeline: source repo origin %q does not match PR base repo %s/%s on %s", baseRemoteURL, req.ReviewPR.Base.Owner, req.ReviewPR.Base.Repo, req.ReviewPR.Base.Host)
	}

	if err := ensureCommit(ctx, deps, req.Artifacts.WorkbenchRepoDir, req.ReviewPR.Base, baseRemoteURL); err != nil {
		return err
	}
	headRemoteURL := baseRemoteURL
	if !sameBranchRepo(req.ReviewPR.Base, req.ReviewPR.Head) {
		headRemoteURL, err = deriveRemoteURL(baseRemoteURL, req.ReviewPR.Head)
		if err != nil {
			return fmt.Errorf("pipeline: derive head remote URL: %w", err)
		}
	}
	if err := ensureCommit(ctx, deps, req.Artifacts.WorkbenchRepoDir, req.ReviewPR.Head, headRemoteURL); err != nil {
		return err
	}
	if _, err := deps.gitCommand(ctx, req.Artifacts.WorkbenchRepoDir, "checkout", "--detach", req.ReviewPR.Head.SHA); err != nil {
		return fmt.Errorf("pipeline: checkout workbench head %s: %w", prref.ShortSHA(req.ReviewPR.Head.SHA), err)
	}
	if err := verifyClean(ctx, deps, req.Artifacts.WorkbenchRepoDir, req.ReviewPR.Head.SHA); err != nil {
		return err
	}

	changedFiles := append([]string(nil), req.ChangedFiles...)
	sort.Strings(changedFiles)
	prIdentity := prIdentity{
		Host:   req.PRRef.Host,
		Owner:  req.PRRef.Owner,
		Repo:   req.PRRef.Repo,
		Number: req.PRRef.Number,
	}
	meta := metadataArtifact{
		SchemaVersion:  metadataSchemaVersion,
		SourceRepoRoot: sourceRepoRoot,
		CheckoutMode:   checkoutModeArtifactClone,
		PR:             prIdentity,
		Base:           branchArtifactFromRef(req.ReviewPR.Base),
		Head:           branchArtifactFromRef(req.ReviewPR.Head),
		RepoPath:       req.Artifacts.WorkbenchRepoDir,
		ScratchPath:    req.Artifacts.WorkbenchScratch,
		ChangedFiles:   changedFiles,
		FingerprintInputs: fingerprintInputs{
			PR:             prIdentity,
			BaseSHA:        req.ReviewPR.Base.SHA,
			HeadSHA:        req.ReviewPR.Head.SHA,
			CheckoutMode:   checkoutModeArtifactClone,
			ChangedFiles:   changedFiles,
			SourceRepoRoot: sourceRepoRoot,
		},
	}
	return writeJSONFile(req.Artifacts.WorkbenchMetadataPath(), meta)
}

func resolveBaseRemoteURL(ctx context.Context, deps Deps, sourceRepoRoot string, base gitprovider.PRBranchRef) (string, error) {
	originOutput, err := deps.gitCommand(ctx, sourceRepoRoot, "remote", "get-url", "origin")
	if err != nil {
		return "", fmt.Errorf("pipeline: resolve source repo origin: %w", err)
	}
	originURL := strings.TrimSpace(string(originOutput))
	if originURL == "" {
		return "", fmt.Errorf("pipeline: source repo origin URL is empty")
	}
	host, owner, repo, _, err := parseRemoteURL(originURL)
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

func remoteMatchesBaseHost(remoteURL string, base gitprovider.PRBranchRef) (bool, error) {
	host, _, _, _, err := parseRemoteURL(remoteURL)
	if err != nil {
		return false, fmt.Errorf("pipeline: parse source repo origin URL %q: %w", remoteURL, err)
	}
	return strings.EqualFold(host, base.Host), nil
}

func ensureCommit(ctx context.Context, deps Deps, repoDir string, branch gitprovider.PRBranchRef, remoteURL string) error {
	ref := strings.TrimSpace(branch.Ref)
	if err := validateFetchRef(ref); err != nil {
		return err
	}
	if commitPresent(ctx, deps, repoDir, branch.SHA) {
		return nil
	}
	if _, err := deps.gitCommand(ctx, repoDir, "fetch", "--no-tags", remoteURL, branch.SHA); err == nil && commitPresent(ctx, deps, repoDir, branch.SHA) {
		return nil
	}
	if ref != "" {
		if _, err := deps.gitCommand(ctx, repoDir, "fetch", "--no-tags", remoteURL, ref); err == nil && commitPresent(ctx, deps, repoDir, branch.SHA) {
			return nil
		}
	}
	return fmt.Errorf("pipeline: fetch commit %s for %s/%s from %q", prref.ShortSHA(branch.SHA), branch.Owner, branch.Repo, remoteURL)
}

func validateFetchRef(ref string) error {
	if ref == "" {
		return nil
	}
	if !strings.HasPrefix(ref, "refs/") {
		return fmt.Errorf("pipeline: reject unsafe fetch ref %q", ref)
	}
	return nil
}

func commitPresent(ctx context.Context, deps Deps, repoDir, sha string) bool {
	if strings.TrimSpace(sha) == "" {
		return false
	}
	_, err := deps.gitCommand(ctx, repoDir, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

func sameBranchRepo(left, right gitprovider.PRBranchRef) bool {
	return strings.EqualFold(left.Host, right.Host) && left.Owner == right.Owner && left.Repo == right.Repo
}

func branchArtifactFromRef(ref gitprovider.PRBranchRef) branchArtifact {
	return branchArtifact{
		Host:  ref.Host,
		Owner: ref.Owner,
		Repo:  ref.Repo,
		Name:  ref.Name,
		Ref:   ref.Ref,
		SHA:   ref.SHA,
	}
}

func verifyClean(ctx context.Context, deps Deps, repoDir string, headSHA string) error {
	head, err := deps.gitCommand(ctx, repoDir, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("pipeline: verify workbench head: %w", err)
	}
	if got := strings.TrimSpace(string(head)); got != strings.TrimSpace(headSHA) {
		return fmt.Errorf("pipeline: workbench head %s does not match expected %s", prref.ShortSHA(got), prref.ShortSHA(headSHA))
	}
	status, err := deps.gitCommand(ctx, repoDir, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("pipeline: verify workbench status: %w", err)
	}
	if strings.TrimSpace(string(status)) != "" {
		return fmt.Errorf("pipeline: workbench has local changes")
	}
	return nil
}

// PrepareReviewerRequest creates a disposable reviewer workspace and LLM request.
func PrepareReviewerRequest(ctx context.Context, deps Deps, adapter llm.Adapter, artifacts runartifact.Paths, headSHA string, agentID string, allowedFiles []string, model, effort, prompt, logPath string) (llm.Request, func() error, error) {
	if err := llm.RequireReviewerWorkspace(adapter); err != nil {
		return llm.Request{}, nil, fmt.Errorf("pipeline: %w", err)
	}
	workspace, cleanup, err := prepareReviewerWorkspace(ctx, deps, artifacts, headSHA, agentID, allowedFiles, defaultReviewerWorkspaceToolOutputBytes)
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
			retryWorkspace, retryCleanup, err := prepareReviewerWorkspace(ctx, deps, artifacts, headSHA, agentID, allowedFiles, defaultReviewerWorkspaceToolOutputBytes)
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

func prepareReviewerWorkspace(ctx context.Context, deps Deps, artifacts runartifact.Paths, headSHA string, agentID string, allowedFiles []string, maxToolOutputBytes int) (llm.ReviewerWorkspaceRequest, func() error, error) {
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
	if _, err := deps.gitCommand(ctx, "", "clone", "--no-hardlinks", artifacts.WorkbenchRepoDir, workspaceRepo); err != nil {
		_ = cleanup()
		return llm.ReviewerWorkspaceRequest{}, nil, fmt.Errorf("pipeline: clone reviewer workspace: %w", err)
	}
	if _, err := deps.gitCommand(ctx, workspaceRepo, "checkout", "--detach", headSHA); err != nil {
		_ = cleanup()
		return llm.ReviewerWorkspaceRequest{}, nil, fmt.Errorf("pipeline: checkout reviewer workspace head %s: %w", prref.ShortSHA(headSHA), err)
	}
	if err := verifyClean(ctx, deps, workspaceRepo, headSHA); err != nil {
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
	return llm.ReviewerWorkspaceRequest{
		RepoDir:            workspaceRepo,
		ScratchDir:         workspaceScratch,
		AllowedFiles:       append([]string(nil), allowedFiles...),
		MaxToolOutputBytes: maxToolOutputBytes,
	}, cleanup, nil
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

type remoteStyle struct {
	scheme string
	user   string
	host   string
	scp    bool
	dotGit bool
}

func parseRemoteURL(raw string) (host, owner, repo string, style remoteStyle, err error) {
	raw = strings.TrimSpace(raw)
	switch {
	case strings.Contains(raw, "://"):
		parsed, parseErr := url.Parse(raw)
		if parseErr != nil {
			return "", "", "", remoteStyle{}, parseErr
		}
		owner, repo, dotGit, parseErr := parseRepoPath(parsed.Path)
		if parseErr != nil {
			return "", "", "", remoteStyle{}, parseErr
		}
		style = remoteStyle{
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
			return "", "", "", remoteStyle{}, fmt.Errorf("invalid scp-style remote")
		}
		userHost := parts[0]
		pathPart := parts[1]
		userHostParts := strings.SplitN(userHost, "@", 2)
		if len(userHostParts) != 2 {
			return "", "", "", remoteStyle{}, fmt.Errorf("invalid scp-style remote")
		}
		owner, repo, dotGit, parseErr := parseRepoPath(pathPart)
		if parseErr != nil {
			return "", "", "", remoteStyle{}, parseErr
		}
		style = remoteStyle{
			user:   userHostParts[0],
			host:   userHostParts[1],
			scp:    true,
			dotGit: dotGit,
		}
		return userHostParts[1], owner, repo, style, nil
	default:
		return "", "", "", remoteStyle{}, fmt.Errorf("unsupported remote URL %q", raw)
	}
}

func parseRepoPath(path string) (owner, repo string, dotGit bool, err error) {
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

func deriveRemoteURL(originURL string, branch gitprovider.PRBranchRef) (string, error) {
	_, _, _, style, err := parseRemoteURL(originURL)
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

func writeJSONFile(path string, payload any) error {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	if err := fsatomic.WriteFileAtomic(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("pipeline: write dossier artifact %s: %w", filepath.Base(path), err)
	}
	return nil
}

func (deps Deps) gitCommand(ctx context.Context, dir string, args ...string) ([]byte, error) {
	if deps.GitCommand != nil {
		return deps.GitCommand(ctx, dir, args...)
	}
	cmdArgs := append([]string{}, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...) // #nosec G204 -- workbench invokes git with fixed command names and structured arguments.
	if strings.TrimSpace(dir) != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), message)
	}
	return output, nil
}

func (deps Deps) resolveRepoRoot(ctx context.Context) (string, error) {
	if deps.ResolveRepoRoot != nil {
		return deps.ResolveRepoRoot(ctx)
	}
	return reporoot.Resolve(ctx, "", deps.GitCommand)
}

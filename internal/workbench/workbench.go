// Package workbench prepares isolated repository workspaces for reviews.
package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/fsatomic"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/prref"
	"github.com/open-cli-collective/codereview-cli/internal/runartifact"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

const (
	metadataSchemaVersion                   = 2
	checkoutModeArtifactClone               = "artifact-clone"
	defaultReviewerWorkspaceToolOutputBytes = 32 * 1024
)

// ErrUnsafeFetchRef marks a provider ref that cannot be passed to Git safely.
var ErrUnsafeFetchRef = errors.New("workbench: unsafe fetch ref")

// ErrInvalidRepositoryIdentity marks repository coordinates that cannot form a safe remote.
var ErrInvalidRepositoryIdentity = errors.New("workbench: invalid repository identity")

// Deps contains the injected repository operations used to prepare workspaces.
type Deps struct {
	GitCommand func(context.Context, string, ...string) ([]byte, error)
}

// RunPreparer creates and validates durable review workbenches.
type RunPreparer struct{ deps Deps }

// NewRunPreparer constructs a durable workbench preparer.
func NewRunPreparer(gitCommand func(context.Context, string, ...string) ([]byte, error)) *RunPreparer {
	return &RunPreparer{deps: Deps{GitCommand: gitCommand}}
}

// Request identifies the review checkout and artifact paths to prepare.
type Request struct {
	PRRef        gitprovider.PRRef
	ReviewPR     gitprovider.PR
	ChangedFiles []string
	Artifacts    runartifact.Paths
	// HeadRefNamespace is the host's virtual ref namespace serving
	// pull-request heads (gitprovider.ProviderCaps.HeadRefNamespace). Empty
	// means the GitHub "pull" namespace.
	HeadRefNamespace string
}

type metadataArtifact struct {
	SchemaVersion     int               `json:"schema_version"`
	SourceRepoRoot    string            `json:"source_repo_root,omitempty"`
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
	PR           prIdentity `json:"pr"`
	BaseSHA      string     `json:"base_sha"`
	HeadSHA      string     `json:"head_sha"`
	CheckoutMode string     `json:"checkout_mode"`
	ChangedFiles []string   `json:"changed_files,omitempty"`
}

// Prepare creates a clean checkout pinned to the requested review commits.
func Prepare(ctx context.Context, deps Deps, req Request) error {
	return NewRunPreparer(deps.GitCommand).Prepare(ctx, req)
}

// Prepare creates or reuses a clean checkout pinned to the requested commits.
func (p *RunPreparer) Prepare(ctx context.Context, req Request) error {
	if reusable, err := p.reusable(ctx, req); err != nil {
		return err
	} else if reusable {
		return nil
	}

	if err := os.RemoveAll(req.Artifacts.WorkbenchDir); err != nil {
		return fmt.Errorf("pipeline: reset workbench dir: %w", err)
	}
	for _, dir := range []string{req.Artifacts.WorkbenchDir, req.Artifacts.WorkbenchScratch} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("pipeline: create workbench dir: %w", err)
		}
	}

	baseRemoteURL, err := branchRemoteURL(req.ReviewPR.Base)
	if err != nil {
		return err
	}
	if _, err := p.deps.gitCommand(ctx, "", "init", req.Artifacts.WorkbenchRepoDir); err != nil {
		return fmt.Errorf("pipeline: initialize workbench repo: %w", err)
	}
	if _, err := p.deps.gitCommand(ctx, req.Artifacts.WorkbenchRepoDir, "remote", "add", "origin", baseRemoteURL); err != nil {
		return fmt.Errorf("pipeline: configure workbench origin: %w", err)
	}

	if err := ensureCommit(ctx, p.deps, req.Artifacts.WorkbenchRepoDir, req.ReviewPR.Base, baseRemoteURL); err != nil {
		return err
	}
	if !sameBranchRepo(req.ReviewPR.Base, req.ReviewPR.Head) {
		if err := ensurePullRequestHead(ctx, p.deps, req.Artifacts.WorkbenchRepoDir, req.PRRef.Number, req.HeadRefNamespace, req.ReviewPR.Head, baseRemoteURL); err != nil {
			return err
		}
	} else if err := ensureCommit(ctx, p.deps, req.Artifacts.WorkbenchRepoDir, req.ReviewPR.Head, baseRemoteURL); err != nil {
		return err
	}
	if _, err := p.deps.gitCommand(ctx, req.Artifacts.WorkbenchRepoDir, "checkout", "--detach", req.ReviewPR.Head.SHA); err != nil {
		return fmt.Errorf("pipeline: checkout workbench head %s: %w", prref.ShortSHA(req.ReviewPR.Head.SHA), err)
	}
	if err := verifyClean(ctx, p.deps, req.Artifacts.WorkbenchRepoDir, req.ReviewPR.Head.SHA); err != nil {
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
		SchemaVersion: metadataSchemaVersion,
		CheckoutMode:  checkoutModeArtifactClone,
		PR:            prIdentity,
		Base:          branchArtifactFromRef(req.ReviewPR.Base),
		Head:          branchArtifactFromRef(req.ReviewPR.Head),
		RepoPath:      req.Artifacts.WorkbenchRepoDir,
		ScratchPath:   req.Artifacts.WorkbenchScratch,
		ChangedFiles:  changedFiles,
		FingerprintInputs: fingerprintInputs{
			PR:           prIdentity,
			BaseSHA:      req.ReviewPR.Base.SHA,
			HeadSHA:      req.ReviewPR.Head.SHA,
			CheckoutMode: checkoutModeArtifactClone,
			ChangedFiles: changedFiles,
		},
	}
	return writeJSONFile(req.Artifacts.WorkbenchMetadataPath(), meta)
}

func (p *RunPreparer) reusable(ctx context.Context, req Request) (bool, error) {
	data, err := os.ReadFile(req.Artifacts.WorkbenchMetadataPath()) // #nosec G304 -- run-owned artifact path.
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("pipeline: read workbench metadata: %w", err)
	}
	var meta metadataArtifact
	if err := json.Unmarshal(data, &meta); err != nil {
		return false, nil
	}
	changedFiles := append([]string(nil), req.ChangedFiles...)
	sort.Strings(changedFiles)
	wantPR := prIdentity{Host: req.PRRef.Host, Owner: req.PRRef.Owner, Repo: req.PRRef.Repo, Number: req.PRRef.Number}
	if (meta.SchemaVersion != 1 && meta.SchemaVersion != metadataSchemaVersion) ||
		meta.CheckoutMode != checkoutModeArtifactClone || meta.PR != wantPR ||
		meta.Base != branchArtifactFromRef(req.ReviewPR.Base) || meta.Head != branchArtifactFromRef(req.ReviewPR.Head) ||
		filepath.Clean(meta.RepoPath) != filepath.Clean(req.Artifacts.WorkbenchRepoDir) ||
		filepath.Clean(meta.ScratchPath) != filepath.Clean(req.Artifacts.WorkbenchScratch) ||
		!slices.Equal(meta.ChangedFiles, changedFiles) ||
		meta.FingerprintInputs.PR != wantPR || meta.FingerprintInputs.BaseSHA != req.ReviewPR.Base.SHA ||
		meta.FingerprintInputs.HeadSHA != req.ReviewPR.Head.SHA || meta.FingerprintInputs.CheckoutMode != checkoutModeArtifactClone ||
		!slices.Equal(meta.FingerprintInputs.ChangedFiles, changedFiles) {
		return false, nil
	}
	if !commitPresent(ctx, p.deps, req.Artifacts.WorkbenchRepoDir, req.ReviewPR.Base.SHA) ||
		!commitPresent(ctx, p.deps, req.Artifacts.WorkbenchRepoDir, req.ReviewPR.Head.SHA) {
		return false, nil
	}
	if err := verifyClean(ctx, p.deps, req.Artifacts.WorkbenchRepoDir, req.ReviewPR.Head.SHA); err != nil {
		return false, nil
	}
	if err := os.MkdirAll(req.Artifacts.WorkbenchScratch, 0o700); err != nil {
		return false, fmt.Errorf("pipeline: create workbench scratch dir: %w", err)
	}
	return true, nil
}

func branchRemoteURL(branch gitprovider.PRBranchRef) (string, error) {
	host := strings.TrimSpace(branch.Host)
	owner := strings.Trim(strings.TrimSpace(branch.Owner), "/")
	repo := strings.TrimSuffix(strings.Trim(strings.TrimSpace(branch.Repo), "/"), ".git")
	// Owners may span nested namespaces on hosts like GitLab, so slashes are
	// allowed there but every segment must still be a plain path element.
	if host == "" || owner == "" || repo == "" || strings.Contains(host, "://") || !validRepoPathSegments(owner) || strings.Contains(repo, "/") || !validRepoPathSegments(repo) {
		return "", fmt.Errorf("%w %s/%s on %s", ErrInvalidRepositoryIdentity, owner, repo, host)
	}
	return (&url.URL{Scheme: "https", Host: host, Path: "/" + owner + "/" + repo + ".git"}).String(), nil
}

func validRepoPathSegments(value string) bool {
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." || segment == "-" {
			return false
		}
	}
	return true
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

func ensurePullRequestHead(ctx context.Context, deps Deps, repoDir string, number int, headRefNamespace string, head gitprovider.PRBranchRef, remoteURL string) error {
	ref, err := pullHeadRef(headRefNamespace, number)
	if err != nil {
		return err
	}
	if _, err := deps.gitCommand(ctx, repoDir, "fetch", "--no-tags", remoteURL, ref); err == nil && commitPresent(ctx, deps, repoDir, head.SHA) {
		return nil
	}
	return fmt.Errorf("pipeline: fetch PR head commit %s from %q ref %q", prref.ShortSHA(head.SHA), remoteURL, ref)
}

func pullHeadRef(namespace string, number int) (string, error) {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		namespace = gitprovider.PullHeadRefNamespace
	}
	for _, r := range namespace {
		if r != '-' && r != '_' && (r < 'a' || r > 'z') {
			return "", fmt.Errorf("%w refs/%s/%d/head", ErrUnsafeFetchRef, namespace, number)
		}
	}
	return fmt.Sprintf("refs/%s/%d/head", namespace, number), nil
}

func validateFetchRef(ref string) error {
	if ref == "" {
		return nil
	}
	if !strings.HasPrefix(ref, "refs/") {
		return fmt.Errorf("%w %q", ErrUnsafeFetchRef, ref)
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
	return strings.EqualFold(left.Host, right.Host) && strings.EqualFold(left.Owner, right.Owner) && strings.EqualFold(left.Repo, right.Repo)
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

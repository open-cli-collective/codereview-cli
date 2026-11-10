package runartifact

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/fsatomic"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

// Git workspaces (the run workbench repo and each reviewer clone) live in a
// flat directory beside the run tree rather than inside it:
//
//	<data>/ws/<key>/r            the git checkout
//	<data>/ws/<key>/owner.json   which run and role own it
//
// The run tree nests five levels under <data>/runs and repeats the PR's host,
// owner and repo, so a checkout inside it leaves little room under Git for
// Windows' 220-character GIT_DIR limit. A workspace's .git path is instead the
// data root plus a fixed 23 characters, whatever the repository, PR or
// reviewer. The key hashes the run directory, role and agent ID, so it is
// stable across resumes of one run and distinct between runs and reviewers.
const (
	// WorkspaceOwnerFile names the ownership record beside each checkout.
	WorkspaceOwnerFile = "owner.json"
	// WorkspaceRoleWorkbench marks the run's canonical workbench checkout.
	WorkspaceRoleWorkbench = "workbench"
	// WorkspaceRoleReviewer marks one reviewer's disposable checkout.
	WorkspaceRoleReviewer = "reviewer"

	workspaceOwnerSchema = 1
	workspaceRepoName    = "r"
	workspaceKeyLength   = 12
	// runDirDepth is how far a run directory sits below <data>/runs:
	// runs/<prKey>/<head>/<base>/<scope>/run-<id>.
	runDirDepth = 5
)

// WorkspaceOwner records which run owns a workspace, so data lifecycle commands
// can remove workspaces whose run artifacts are gone.
type WorkspaceOwner struct {
	SchemaVersion int    `json:"schema_version"`
	RunDir        string `json:"run_dir"`
	Role          string `json:"role"`
	AgentID       string `json:"agent_id,omitempty"`
}

// WorkbenchWorkspace returns the workspace directory holding the run's
// workbench repo, or "" when the repo lives inside WorkbenchDir.
func (p Paths) WorkbenchWorkspace() string {
	if p.WorkspacesRoot == "" {
		return ""
	}
	return filepath.Dir(p.WorkbenchRepoDir)
}

// ReviewerWorkspace returns the workspace root and checkout directory for one
// reviewer. legacySegment names the reviewer's directory when the run has no
// workspaces root and the checkout stays under WorkbenchDir.
func (p Paths) ReviewerWorkspace(agentID, legacySegment string) (root, repo string) {
	if p.WorkspacesRoot == "" {
		root = filepath.Join(p.WorkbenchDir, "reviewers", legacySegment)
		return root, filepath.Join(root, "repo")
	}
	root = workspaceDir(p.WorkspacesRoot, p.Dir, WorkspaceRoleReviewer, agentID)
	return root, filepath.Join(root, workspaceRepoName)
}

// WriteWorkspaceOwner records the owning run for a workspace directory. It is a
// no-op for runs whose workspaces live inside the run tree, since removing the
// run already removes them.
func (p Paths) WriteWorkspaceOwner(workspace, role, agentID string) error {
	if p.WorkspacesRoot == "" {
		return nil
	}
	data, err := json.MarshalIndent(WorkspaceOwner{
		SchemaVersion: workspaceOwnerSchema,
		RunDir:        filepath.Clean(p.Dir),
		Role:          role,
		AgentID:       agentID,
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := fsatomic.WriteFileAtomic(filepath.Join(workspace, WorkspaceOwnerFile), append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("runartifact: write workspace owner: %w", err)
	}
	return nil
}

// ReadWorkspaceOwner reads the ownership record of one workspace directory.
func ReadWorkspaceOwner(workspace string) (WorkspaceOwner, error) {
	path := filepath.Join(workspace, WorkspaceOwnerFile)
	data, err := os.ReadFile(path) // #nosec G304 -- workspace paths are listed from the cr-owned workspaces root.
	if err != nil {
		return WorkspaceOwner{}, err
	}
	var owner WorkspaceOwner
	if err := json.Unmarshal(data, &owner); err != nil {
		return WorkspaceOwner{}, fmt.Errorf("runartifact: %s: %w", path, err)
	}
	if owner.SchemaVersion != workspaceOwnerSchema || strings.TrimSpace(owner.RunDir) == "" {
		return WorkspaceOwner{}, fmt.Errorf("runartifact: %s: invalid workspace owner", path)
	}
	return owner, nil
}

// workspacesRootForRunDir returns <data>/ws for a run directory in the standard
// <data>/runs/.../run-<id> layout, and "" for any other directory, such as a
// caller-owned benchmark or test directory.
func workspacesRootForRunDir(dir string) string {
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	clean := filepath.Clean(dir)
	if !strings.HasPrefix(filepath.Base(clean), "run-") {
		return ""
	}
	runs := clean
	for range runDirDepth {
		runs = filepath.Dir(runs)
	}
	if filepath.Base(runs) != "runs" {
		return ""
	}
	return filepath.Join(filepath.Dir(runs), statepaths.WorkspacesDirName)
}

func workspaceDir(root, runDir, role, agentID string) string {
	hash := sha256.New()
	for _, part := range []string{filepath.Clean(runDir), role, agentID} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		hash.Write(size[:])
		hash.Write([]byte(part))
	}
	return filepath.Join(root, hex.EncodeToString(hash.Sum(nil))[:workspaceKeyLength])
}

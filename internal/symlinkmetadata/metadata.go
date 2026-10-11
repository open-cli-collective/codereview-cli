// Package symlinkmetadata defines the run-owned, pinned Git evidence used to
// inspect changed symlink payloads without following them.
package symlinkmetadata

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	// SchemaVersion identifies the stable JSON shape for pinned symlink evidence.
	SchemaVersion = 1
	// MaxPayloadBytes bounds Git symlink blobs before their content is read.
	MaxPayloadBytes = 4096
	// MaxArtifactBytes bounds reviewer-tool parsing of the run-owned artifact.
	MaxArtifactBytes = 16 * 1024 * 1024
)

const (
	// ResolutionRegular identifies a lexical target that is a regular blob in
	// the same pinned Git tree as its symlink.
	ResolutionRegular = "regular"
	// ResolutionMissing identifies a target absent from the pinned Git tree.
	ResolutionMissing = "missing"
	// ResolutionLinkedTarget identifies a target that is itself a symlink.
	ResolutionLinkedTarget = "linked_target"
	// ResolutionLinkedAncestor identifies a path below a symlink ancestor.
	ResolutionLinkedAncestor = "linked_ancestor"
	// ResolutionOutside identifies an absolute or escaping target path.
	ResolutionOutside = "outside_repository"
	// ResolutionUnsupported identifies payloads or target forms not resolved
	// by the safe lexical classifier.
	ResolutionUnsupported = "unsupported"
)

// Artifact records exact changed-link payloads and lexical resolution against
// both pinned Git trees. It never contains a target file body.
type Artifact struct {
	SchemaVersion int    `json:"schema_version"`
	BaseSHA       string `json:"base_sha"`
	HeadSHA       string `json:"head_sha"`
	Links         []Link `json:"links"`
	Digest        string `json:"digest"`
}

// Link identifies a changed path and its pinned base/head symlink metadata.
type Link struct {
	OldPath string `json:"old_path,omitempty"`
	Path    string `json:"path"`
	Base    *Side  `json:"base,omitempty"`
	Head    *Side  `json:"head,omitempty"`
}

// Side is one pinned symlink object and the safe lexical resolution of its
// payload. PayloadBase64 preserves exact bytes when the target is not UTF-8.
type Side struct {
	Path                 string `json:"path"`
	Mode                 string `json:"mode"`
	BlobOID              string `json:"blob_oid"`
	Payload              string `json:"payload"`
	PayloadBase64        string `json:"payload_base64,omitempty"`
	PayloadSize          int    `json:"payload_size"`
	PayloadOmittedReason string `json:"payload_omitted_reason,omitempty"`
	Resolution           string `json:"resolution"`
	ResolvedPath         string `json:"resolved_path,omitempty"`
	TargetMode           string `json:"target_mode,omitempty"`
	TargetBlobOID        string `json:"target_blob_oid,omitempty"`
}

// New builds a deterministic, digest-bound artifact.
func New(baseSHA, headSHA string, links []Link) (Artifact, error) {
	artifact := Artifact{
		SchemaVersion: SchemaVersion,
		BaseSHA:       baseSHA,
		HeadSHA:       headSHA,
		Links:         append([]Link(nil), links...),
	}
	sort.Slice(artifact.Links, func(i, j int) bool {
		if artifact.Links[i].Path == artifact.Links[j].Path {
			return artifact.Links[i].OldPath < artifact.Links[j].OldPath
		}
		return artifact.Links[i].Path < artifact.Links[j].Path
	})
	if err := artifact.validateShape(); err != nil {
		return Artifact{}, err
	}
	digest, err := artifact.canonicalDigest()
	if err != nil {
		return Artifact{}, err
	}
	artifact.Digest = digest
	return artifact, nil
}

// Marshal serializes the complete artifact in a stable struct field order.
func (artifact Artifact) Marshal() ([]byte, error) {
	if err := artifact.validateShape(); err != nil {
		return nil, err
	}
	digest, err := artifact.canonicalDigest()
	if err != nil {
		return nil, err
	}
	if artifact.Digest == "" || artifact.Digest != digest {
		return nil, errors.New("symlinkmetadata: artifact digest mismatch")
	}
	return json.MarshalIndent(artifact, "", "  ")
}

// Decode validates strict JSON, the artifact's own digest, the expected pinned
// identity and digest, and the exact changed path requested by the reviewer.
func Decode(data []byte, expectedDigest, baseSHA, headSHA, requestedPath string) (Artifact, Link, error) {
	var zero Artifact
	if len(data) == 0 || len(data) > MaxArtifactBytes {
		return zero, Link{}, errors.New("symlinkmetadata: artifact size is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var artifact Artifact
	if err := decoder.Decode(&artifact); err != nil {
		return zero, Link{}, fmt.Errorf("symlinkmetadata: decode artifact: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return zero, Link{}, errors.New("symlinkmetadata: multiple JSON values")
		}
		return zero, Link{}, fmt.Errorf("symlinkmetadata: trailing artifact data: %w", err)
	}
	if err := artifact.validateShape(); err != nil {
		return zero, Link{}, err
	}
	digest, err := artifact.canonicalDigest()
	if err != nil {
		return zero, Link{}, err
	}
	if artifact.Digest != digest || expectedDigest == "" || artifact.Digest != expectedDigest {
		return zero, Link{}, errors.New("symlinkmetadata: artifact digest mismatch")
	}
	if artifact.BaseSHA != baseSHA || artifact.HeadSHA != headSHA {
		return zero, Link{}, errors.New("symlinkmetadata: pinned identity mismatch")
	}
	for _, link := range artifact.Links {
		if link.Path == requestedPath {
			return artifact, link, nil
		}
	}
	return zero, Link{}, errors.New("symlinkmetadata: requested path is not an exact changed symlink path")
}

func (artifact Artifact) canonicalDigest() (string, error) {
	canonical, err := json.Marshal(struct {
		SchemaVersion int    `json:"schema_version"`
		BaseSHA       string `json:"base_sha"`
		HeadSHA       string `json:"head_sha"`
		Links         []Link `json:"links"`
	}{artifact.SchemaVersion, artifact.BaseSHA, artifact.HeadSHA, artifact.Links})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func (artifact Artifact) validateShape() error {
	if artifact.SchemaVersion != SchemaVersion || !fullOID(artifact.BaseSHA) || !fullOID(artifact.HeadSHA) {
		return errors.New("symlinkmetadata: schema or pinned identity is invalid")
	}
	seen := make(map[string]bool, len(artifact.Links))
	for _, link := range artifact.Links {
		if !safeGitPath(link.Path) || (link.OldPath != "" && !safeGitPath(link.OldPath)) || seen[link.Path] {
			return errors.New("symlinkmetadata: changed path is invalid or duplicated")
		}
		seen[link.Path] = true
		if link.Base == nil && link.Head == nil {
			return errors.New("symlinkmetadata: changed link has no pinned side")
		}
		if link.Base != nil && !validSide(*link.Base, link.OldPath) {
			return errors.New("symlinkmetadata: base symlink metadata is invalid")
		}
		if link.Head != nil && !validSide(*link.Head, link.Path) {
			return errors.New("symlinkmetadata: head symlink metadata is invalid")
		}
	}
	return nil
}

func validSide(side Side, expectedPath string) bool {
	if side.Path != expectedPath || side.Mode != "120000" || !fullOID(side.BlobOID) {
		return false
	}
	var payload []byte
	if side.PayloadBase64 != "" {
		decoded, err := base64.StdEncoding.DecodeString(side.PayloadBase64)
		if err != nil || len(decoded) > MaxPayloadBytes || (utf8.Valid(decoded) && side.Payload != string(decoded)) {
			return false
		}
		payload = decoded
	} else {
		if !utf8.ValidString(side.Payload) || len(side.Payload) > MaxPayloadBytes {
			return false
		}
		payload = []byte(side.Payload)
	}
	if side.PayloadOmittedReason != "" {
		return side.PayloadOmittedReason == "oversize" && side.PayloadSize > MaxPayloadBytes && len(payload) == 0 && side.Payload == "" && side.PayloadBase64 == "" && side.Resolution == ResolutionUnsupported && side.TargetMode == "" && side.TargetBlobOID == ""
	}
	if side.PayloadSize != len(payload) {
		return false
	}
	if side.PayloadSize > MaxPayloadBytes {
		return false
	}
	if len(payload) == 0 {
		return side.Resolution == ResolutionUnsupported
	}
	if strings.IndexByte(string(payload), 0) >= 0 {
		return side.Resolution == ResolutionUnsupported
	}
	switch side.Resolution {
	case ResolutionRegular:
		return safeGitPath(side.ResolvedPath) && (side.TargetMode == "100644" || side.TargetMode == "100755") && fullOID(side.TargetBlobOID)
	case ResolutionMissing, ResolutionLinkedTarget, ResolutionLinkedAncestor, ResolutionOutside, ResolutionUnsupported:
		return side.TargetMode == "" && side.TargetBlobOID == ""
	default:
		return false
	}
}

func safeGitPath(value string) bool {
	if value == "" || strings.HasPrefix(value, "/") || strings.ContainsRune(value, '\x00') {
		return false
	}
	clean := path.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != value {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." || component == ".git" {
			return false
		}
	}
	return true
}

func fullOID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	if strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

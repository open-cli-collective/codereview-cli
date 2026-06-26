package gitprovider

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

type fileSelector struct {
	Ref    PRRef
	GitRef string
	Path   string
}

type reviewAuthoritySelector struct {
	Ref   PRRef
	Login string
}

// ThreadReply records one ReplyToThread write.
type ThreadReply struct {
	ThreadID ThreadID
	Body     string
}

// Fake is an importable, concurrency-safe GitProvider test double.
type Fake struct {
	mu sync.Mutex

	caps     ProviderCaps
	identity Identity
	errors   map[Operation]error

	prs           map[PRRef]PR
	diffs         map[PRRef]UnifiedDiff
	files         map[fileSelector][]byte
	trees         map[fileSelector][]TreeEntry
	threads       map[PRRef][]InlineThread
	reviews       map[PRRef][]Review
	issueComments map[PRRef][]IssueComment
	reviewAuth    map[reviewAuthoritySelector]ReviewAuthority

	inlineCommentWrites map[PRRef][]InlineComment
	threadReplyWrites   map[PRRef][]ThreadReply
	resolveThreadWrites map[PRRef][]ThreadID
	issueCommentWrites  map[PRRef][]string
	reviewWrites        map[PRRef][]ReviewRequest

	nextCommentID int
	nextReviewID  int
}

// SetCapabilities configures the capabilities returned by Capabilities.
func (f *Fake) SetCapabilities(caps ProviderCaps) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.caps = caps
}

// SetIdentity configures the identity returned by WhoAmI.
func (f *Fake) SetIdentity(identity Identity) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.identity = identity
}

// SetError injects err for op. Passing nil clears the injected error.
func (f *Fake) SetError(op Operation, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureLocked()
	if err == nil {
		delete(f.errors, op)
		return
	}
	f.errors[op] = err
}

// SetPR configures the PR snapshot for ref.
func (f *Fake) SetPR(ref PRRef, pr PR) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureLocked()
	f.prs[ref] = pr
	return nil
}

// SetDiff configures the unified diff for ref.
func (f *Fake) SetDiff(ref PRRef, diff UnifiedDiff) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureLocked()
	f.diffs[ref] = diff
	return nil
}

// SetFileAtRef configures file contents for a full ref/path selector.
func (f *Fake) SetFileAtRef(ref PRRef, gitRef, path string, contents []byte) error {
	selector, err := newFileSelector(ref, gitRef, path)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureLocked()
	f.files[selector] = append([]byte(nil), contents...)
	return nil
}

// SetTreeAtRef configures tree entries for a full ref/path selector.
func (f *Fake) SetTreeAtRef(ref PRRef, gitRef, path string, entries []TreeEntry) error {
	selector, err := newFileSelector(ref, gitRef, path)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureLocked()
	f.trees[selector] = append([]TreeEntry(nil), entries...)
	return nil
}

// SetInlineThreads configures inline threads for ref.
func (f *Fake) SetInlineThreads(ref PRRef, threads []InlineThread) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureLocked()
	f.threads[ref] = copyThreads(threads)
	return nil
}

// SetReviews configures reviews for ref.
func (f *Fake) SetReviews(ref PRRef, reviews []Review) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureLocked()
	f.reviews[ref] = append([]Review(nil), reviews...)
	return nil
}

// SetIssueComments configures issue comments for ref.
func (f *Fake) SetIssueComments(ref PRRef, comments []IssueComment) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureLocked()
	f.issueComments[ref] = append([]IssueComment(nil), comments...)
	return nil
}

// SetReviewAuthority configures the authority lookup for ref/login.
func (f *Fake) SetReviewAuthority(ref PRRef, login string, authority ReviewAuthority) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(login) == "" {
		return fmt.Errorf("login is required")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureLocked()
	f.reviewAuth[reviewAuthoritySelector{Ref: ref, Login: strings.TrimSpace(login)}] = authority
	return nil
}

// WhoAmI returns the configured fake identity or an injected error.
func (f *Fake) WhoAmI(_ context.Context, _ Credential) (Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureLocked()
	if err := f.errorLocked(OperationWhoAmI); err != nil {
		return Identity{}, err
	}
	return f.identity, nil
}

// ReviewAuthority returns the configured authority for ref/login.
func (f *Fake) ReviewAuthority(_ context.Context, ref PRRef, identity Identity) (ReviewAuthority, error) {
	if err := ref.Validate(); err != nil {
		return ReviewAuthority{}, err
	}
	login := strings.TrimSpace(identity.Login)
	if login == "" {
		return ReviewAuthority{}, fmt.Errorf("identity login is required")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureLocked()
	if err := f.errorLocked(OperationReviewAuthority); err != nil {
		return ReviewAuthority{}, err
	}
	return f.reviewAuth[reviewAuthoritySelector{Ref: ref, Login: login}], nil
}

// GetPR returns the canned PR snapshot for ref.
func (f *Fake) GetPR(_ context.Context, ref PRRef) (PR, error) {
	if err := ref.Validate(); err != nil {
		return PR{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureLocked()
	if err := f.errorLocked(OperationGetPR); err != nil {
		return PR{}, err
	}
	pr, ok := f.prs[ref]
	if !ok {
		return PR{}, WrapError(ErrNotFound, OperationGetPR, nil)
	}
	return pr, nil
}

// GetDiff returns the canned unified diff for ref.
func (f *Fake) GetDiff(_ context.Context, ref PRRef) (UnifiedDiff, error) {
	if err := ref.Validate(); err != nil {
		return UnifiedDiff{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureLocked()
	if err := f.errorLocked(OperationGetDiff); err != nil {
		return UnifiedDiff{}, err
	}
	diff, ok := f.diffs[ref]
	if !ok {
		return UnifiedDiff{}, WrapError(ErrNotFound, OperationGetDiff, nil)
	}
	return diff, nil
}

// GetFileAtRef returns the canned file contents for the full ref/path selector.
func (f *Fake) GetFileAtRef(_ context.Context, ref PRRef, gitRef string, path string) ([]byte, error) {
	selector, err := newFileSelector(ref, gitRef, path)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureLocked()
	if err := f.errorLocked(OperationGetFileAtRef); err != nil {
		return nil, err
	}
	contents, ok := f.files[selector]
	if !ok {
		return nil, WrapError(ErrNotFound, OperationGetFileAtRef, nil)
	}
	return append([]byte(nil), contents...), nil
}

// ListTreeAtRef returns the canned tree entries for the full ref/path selector.
func (f *Fake) ListTreeAtRef(_ context.Context, ref PRRef, gitRef string, path string) ([]TreeEntry, error) {
	selector, err := newFileSelector(ref, gitRef, path)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureLocked()
	if err := f.errorLocked(OperationListTreeAtRef); err != nil {
		return nil, err
	}
	entries, ok := f.trees[selector]
	if !ok {
		return nil, WrapError(ErrNotFound, OperationListTreeAtRef, nil)
	}
	return append([]TreeEntry(nil), entries...), nil
}

// ListInlineThreads returns the canned inline threads for ref.
func (f *Fake) ListInlineThreads(_ context.Context, ref PRRef) ([]InlineThread, error) {
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureLocked()
	if err := f.errorLocked(OperationListInlineThreads); err != nil {
		return nil, err
	}
	return copyThreads(f.threads[ref]), nil
}

// ListReviews returns the canned reviews for ref.
func (f *Fake) ListReviews(_ context.Context, ref PRRef) ([]Review, error) {
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureLocked()
	if err := f.errorLocked(OperationListReviews); err != nil {
		return nil, err
	}
	return append([]Review(nil), f.reviews[ref]...), nil
}

// ListIssueComments returns the canned issue comments for ref.
func (f *Fake) ListIssueComments(_ context.Context, ref PRRef) ([]IssueComment, error) {
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureLocked()
	if err := f.errorLocked(OperationListIssueComments); err != nil {
		return nil, err
	}
	return append([]IssueComment(nil), f.issueComments[ref]...), nil
}

// PostInlineComment validates and records an inline comment write.
func (f *Fake) PostInlineComment(_ context.Context, ref PRRef, c InlineComment) (CommentID, error) {
	if err := ref.Validate(); err != nil {
		return "", err
	}
	if err := c.Validate(); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureLocked()
	if err := f.errorLocked(OperationPostInlineComment); err != nil {
		return "", err
	}
	f.inlineCommentWrites[ref] = append(f.inlineCommentWrites[ref], c)
	return f.nextCommentIDLocked(), nil
}

// ReplyToThread validates and records a thread reply write.
func (f *Fake) ReplyToThread(_ context.Context, ref PRRef, threadID ThreadID, body string) (CommentID, error) {
	if err := ref.Validate(); err != nil {
		return "", err
	}
	if err := requireNonEmpty("thread ID", string(threadID)); err != nil {
		return "", err
	}
	if err := requireNonEmpty("body", body); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureLocked()
	if err := f.errorLocked(OperationReplyToThread); err != nil {
		return "", err
	}
	f.threadReplyWrites[ref] = append(f.threadReplyWrites[ref], ThreadReply{ThreadID: threadID, Body: body})
	return f.nextCommentIDLocked(), nil
}

// ResolveThread validates and records a thread resolution write.
func (f *Fake) ResolveThread(_ context.Context, ref PRRef, threadID ThreadID) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if err := requireNonEmpty("thread ID", string(threadID)); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureLocked()
	if err := f.errorLocked(OperationResolveThread); err != nil {
		return err
	}
	f.resolveThreadWrites[ref] = append(f.resolveThreadWrites[ref], threadID)
	return nil
}

// PostIssueComment validates and records an issue comment write.
func (f *Fake) PostIssueComment(_ context.Context, ref PRRef, body string) (CommentID, error) {
	if err := ref.Validate(); err != nil {
		return "", err
	}
	if err := requireNonEmpty("body", body); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureLocked()
	if err := f.errorLocked(OperationPostIssueComment); err != nil {
		return "", err
	}
	f.issueCommentWrites[ref] = append(f.issueCommentWrites[ref], body)
	return f.nextCommentIDLocked(), nil
}

// SubmitReview validates and records a review submission write.
func (f *Fake) SubmitReview(_ context.Context, ref PRRef, r ReviewRequest) (ReviewID, error) {
	if err := ref.Validate(); err != nil {
		return "", err
	}
	if err := r.Validate(); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureLocked()
	if err := f.errorLocked(OperationSubmitReview); err != nil {
		return "", err
	}
	f.reviewWrites[ref] = append(f.reviewWrites[ref], r)
	return f.nextReviewIDLocked(), nil
}

// Capabilities returns the configured fake provider capabilities.
func (f *Fake) Capabilities() ProviderCaps {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.caps
}

// RecordedInlineComments returns successful inline-comment writes for ref.
func (f *Fake) RecordedInlineComments(ref PRRef) []InlineComment {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]InlineComment(nil), f.inlineCommentWrites[ref]...)
}

// RecordedThreadReplies returns successful thread replies for ref.
func (f *Fake) RecordedThreadReplies(ref PRRef) []ThreadReply {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ThreadReply(nil), f.threadReplyWrites[ref]...)
}

// RecordedResolvedThreads returns successful thread resolutions for ref.
func (f *Fake) RecordedResolvedThreads(ref PRRef) []ThreadID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ThreadID(nil), f.resolveThreadWrites[ref]...)
}

// RecordedIssueComments returns successful issue-comment writes for ref.
func (f *Fake) RecordedIssueComments(ref PRRef) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.issueCommentWrites[ref]...)
}

// RecordedReviews returns successful review submissions for ref.
func (f *Fake) RecordedReviews(ref PRRef) []ReviewRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ReviewRequest(nil), f.reviewWrites[ref]...)
}

func (f *Fake) ensureLocked() {
	if f.errors == nil {
		f.errors = make(map[Operation]error)
	}
	if f.prs == nil {
		f.prs = make(map[PRRef]PR)
	}
	if f.diffs == nil {
		f.diffs = make(map[PRRef]UnifiedDiff)
	}
	if f.files == nil {
		f.files = make(map[fileSelector][]byte)
	}
	if f.trees == nil {
		f.trees = make(map[fileSelector][]TreeEntry)
	}
	if f.threads == nil {
		f.threads = make(map[PRRef][]InlineThread)
	}
	if f.reviews == nil {
		f.reviews = make(map[PRRef][]Review)
	}
	if f.issueComments == nil {
		f.issueComments = make(map[PRRef][]IssueComment)
	}
	if f.reviewAuth == nil {
		f.reviewAuth = make(map[reviewAuthoritySelector]ReviewAuthority)
	}
	if f.inlineCommentWrites == nil {
		f.inlineCommentWrites = make(map[PRRef][]InlineComment)
	}
	if f.threadReplyWrites == nil {
		f.threadReplyWrites = make(map[PRRef][]ThreadReply)
	}
	if f.resolveThreadWrites == nil {
		f.resolveThreadWrites = make(map[PRRef][]ThreadID)
	}
	if f.issueCommentWrites == nil {
		f.issueCommentWrites = make(map[PRRef][]string)
	}
	if f.reviewWrites == nil {
		f.reviewWrites = make(map[PRRef][]ReviewRequest)
	}
}

func (f *Fake) errorLocked(op Operation) error {
	return f.errors[op]
}

func (f *Fake) nextCommentIDLocked() CommentID {
	f.nextCommentID++
	return CommentID(fmt.Sprintf("fake-comment-%d", f.nextCommentID))
}

func (f *Fake) nextReviewIDLocked() ReviewID {
	f.nextReviewID++
	return ReviewID(fmt.Sprintf("fake-review-%d", f.nextReviewID))
}

func newFileSelector(ref PRRef, gitRef, path string) (fileSelector, error) {
	if err := ref.Validate(); err != nil {
		return fileSelector{}, err
	}
	if err := requireNonEmpty("git ref", gitRef); err != nil {
		return fileSelector{}, err
	}
	if err := requireNonEmpty("path", path); err != nil {
		return fileSelector{}, err
	}
	return fileSelector{Ref: ref, GitRef: gitRef, Path: path}, nil
}

func copyThreads(threads []InlineThread) []InlineThread {
	copied := append([]InlineThread(nil), threads...)
	for i := range copied {
		copied[i].Comments = append([]ThreadComment(nil), copied[i].Comments...)
	}
	return copied
}

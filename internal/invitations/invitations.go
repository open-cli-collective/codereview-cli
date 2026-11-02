// Package invitations selects and accepts the pending repository invitations
// of one git-host identity against an explicit owner/repository allowlist.
package invitations

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
)

// ErrInvalidAllowlist identifies an allowlist entry that is empty or malformed.
var ErrInvalidAllowlist = errors.New("invitations: invalid allowlist")

// ErrAllowlistRequired identifies an accept request without any allowlist entry.
var ErrAllowlistRequired = errors.New("invitations: accept requires at least one allowlisted owner or repository")

// Allowlist names the repository owners and owner/name repositories whose
// invitations may be accepted. Matching is case-insensitive, as host logins
// and repository names are.
type Allowlist struct {
	Owners []string
	Repos  []string
}

// NewAllowlist validates and normalizes owner logins and owner/name
// repositories. Both may be empty; Accept separately requires a non-empty one.
func NewAllowlist(owners, repos []string) (Allowlist, error) {
	var allow Allowlist
	for _, owner := range owners {
		owner = strings.TrimSpace(owner)
		if owner == "" || strings.Contains(owner, "/") || strings.IndexFunc(owner, unicode.IsSpace) >= 0 {
			return Allowlist{}, fmt.Errorf("%w: owner %q must be a single account login", ErrInvalidAllowlist, owner)
		}
		allow.Owners = appendUnique(allow.Owners, owner)
	}
	for _, repo := range repos {
		repo = strings.TrimSpace(repo)
		owner, name, ok := strings.Cut(repo, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") || strings.IndexFunc(repo, unicode.IsSpace) >= 0 {
			return Allowlist{}, fmt.Errorf("%w: repository %q must be owner/name", ErrInvalidAllowlist, repo)
		}
		allow.Repos = appendUnique(allow.Repos, repo)
	}
	return allow, nil
}

// Empty reports whether the allowlist names nothing.
func (a Allowlist) Empty() bool {
	return len(a.Owners) == 0 && len(a.Repos) == 0
}

// Matches reports whether the invitation's repository is on the allowlist.
func (a Allowlist) Matches(invitation gitprovider.RepositoryInvitation) bool {
	for _, owner := range a.Owners {
		if strings.EqualFold(owner, invitation.Owner) {
			return true
		}
	}
	fullName := invitation.FullName()
	for _, repo := range a.Repos {
		if strings.EqualFold(repo, fullName) {
			return true
		}
	}
	return false
}

// List returns the pending invitations, sorted by repository. A non-empty
// filter keeps only matching invitations.
func List(ctx context.Context, provider gitprovider.RepositoryInvitations, filter Allowlist) ([]gitprovider.RepositoryInvitation, error) {
	pending, err := provider.ListRepositoryInvitations(ctx)
	if err != nil {
		return nil, err
	}
	sortInvitations(pending)
	if filter.Empty() {
		return pending, nil
	}
	matched := make([]gitprovider.RepositoryInvitation, 0, len(pending))
	for _, invitation := range pending {
		if filter.Matches(invitation) {
			matched = append(matched, invitation)
		}
	}
	return matched, nil
}

// Status is the outcome of one invitation in an accept run.
type Status string

// Accept outcomes.
const (
	StatusAccepted    Status = "accepted"
	StatusWouldAccept Status = "would_accept"
	StatusSkipped     Status = "skipped"
	StatusFailed      Status = "failed"
)

// Skip reasons.
const (
	ReasonNotAllowlisted = "not on the allowlist"
	ReasonExpired        = "invitation expired; ask the inviter to send a new one"
)

// Outcome records what an accept run did with one pending invitation.
type Outcome struct {
	Invitation gitprovider.RepositoryInvitation
	Status     Status
	Reason     string
	Err        error
}

// Summary counts outcomes by status.
type Summary struct {
	Accepted    int
	WouldAccept int
	Skipped     int
	Failed      int
}

// Summarize counts outcomes by status.
func Summarize(outcomes []Outcome) Summary {
	var summary Summary
	for _, outcome := range outcomes {
		switch outcome.Status {
		case StatusAccepted:
			summary.Accepted++
		case StatusWouldAccept:
			summary.WouldAccept++
		case StatusSkipped:
			summary.Skipped++
		case StatusFailed:
			summary.Failed++
		}
	}
	return summary
}

// Accept accepts every pending, unexpired invitation on the allowlist and
// reports an outcome for every pending invitation, accepted or not. With
// dryRun it reports what it would accept and makes no writes. One failed
// accept does not stop the others; callers inspect the outcomes. The returned
// error is reserved for a missing allowlist or a failed listing.
func Accept(ctx context.Context, provider gitprovider.RepositoryInvitations, allow Allowlist, dryRun bool) ([]Outcome, error) {
	if allow.Empty() {
		return nil, ErrAllowlistRequired
	}
	pending, err := List(ctx, provider, Allowlist{})
	if err != nil {
		return nil, err
	}
	outcomes := make([]Outcome, 0, len(pending))
	for _, invitation := range pending {
		outcome := Outcome{Invitation: invitation}
		switch {
		case !allow.Matches(invitation):
			outcome.Status = StatusSkipped
			outcome.Reason = ReasonNotAllowlisted
		case invitation.Expired:
			outcome.Status = StatusSkipped
			outcome.Reason = ReasonExpired
		case dryRun:
			outcome.Status = StatusWouldAccept
		default:
			if err := provider.AcceptRepositoryInvitation(ctx, invitation.ID); err != nil {
				outcome.Status = StatusFailed
				outcome.Reason = err.Error()
				outcome.Err = err
			} else {
				outcome.Status = StatusAccepted
			}
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes, nil
}

func sortInvitations(invitations []gitprovider.RepositoryInvitation) {
	sort.SliceStable(invitations, func(i, j int) bool {
		left := strings.ToLower(invitations[i].FullName())
		right := strings.ToLower(invitations[j].FullName())
		if left != right {
			return left < right
		}
		return invitations[i].ID < invitations[j].ID
	})
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if strings.EqualFold(existing, value) {
			return values
		}
	}
	return append(values, value)
}

package view

import (
	"fmt"
	"io"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/invitations"
)

// InvitesIdentity describes the git-host identity whose invitations are shown.
type InvitesIdentity struct {
	Profile          string `json:"profile"`
	CredentialSource string `json:"credential_source"`
	Host             string `json:"host"`
	Login            string `json:"login"`
}

// InvitesList is the presentation model for `cr invites list`.
type InvitesList struct {
	InvitesIdentity
	Invitations []InviteItem `json:"invitations"`
}

// InvitesAccept is the presentation model for `cr invites accept`.
type InvitesAccept struct {
	InvitesIdentity
	DryRun    bool                `json:"dry_run"`
	Allowlist InvitesAllowlist    `json:"allowlist"`
	Results   []InviteAcceptItem  `json:"results"`
	Summary   InvitesAcceptCounts `json:"summary"`
}

// InvitesAllowlist echoes the owners and repositories accept was limited to.
type InvitesAllowlist struct {
	Owners []string `json:"owners"`
	Repos  []string `json:"repos"`
}

// InviteItem describes one pending repository invitation.
type InviteItem struct {
	ID         string    `json:"id"`
	Repository string    `json:"repository"`
	Inviter    string    `json:"inviter"`
	Permission string    `json:"permission"`
	CreatedAt  time.Time `json:"created_at"`
	Expired    bool      `json:"expired"`
	URL        string    `json:"url,omitempty"`
}

// InviteAcceptItem describes what accept did with one pending invitation.
// ReasonCode is the stable value for scripts; Reason is the human text.
type InviteAcceptItem struct {
	InviteItem
	Status     string `json:"status"`
	ReasonCode string `json:"reason_code,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// InvitesAcceptCounts counts accept outcomes by status.
type InvitesAcceptCounts struct {
	Accepted    int `json:"accepted"`
	WouldAccept int `json:"would_accept"`
	Skipped     int `json:"skipped"`
	Failed      int `json:"failed"`
}

// NewInvitesList builds the invites list presentation model.
func NewInvitesList(identity InvitesIdentity, pending []gitprovider.RepositoryInvitation) InvitesList {
	items := make([]InviteItem, 0, len(pending))
	for _, invitation := range pending {
		items = append(items, newInviteItem(invitation))
	}
	return InvitesList{InvitesIdentity: identity, Invitations: items}
}

// NewInvitesAccept builds the invites accept presentation model.
func NewInvitesAccept(identity InvitesIdentity, allow invitations.Allowlist, dryRun bool, outcomes []invitations.Outcome) InvitesAccept {
	results := make([]InviteAcceptItem, 0, len(outcomes))
	for _, outcome := range outcomes {
		results = append(results, InviteAcceptItem{
			InviteItem: newInviteItem(outcome.Invitation),
			Status:     string(outcome.Status),
			ReasonCode: string(outcome.Reason),
			Reason:     inviteReasonText(outcome),
		})
	}
	summary := invitations.Summarize(outcomes)
	return InvitesAccept{
		InvitesIdentity: identity,
		DryRun:          dryRun,
		Allowlist: InvitesAllowlist{
			Owners: append([]string{}, allow.Owners...),
			Repos:  append([]string{}, allow.Repos...),
		},
		Results: results,
		Summary: InvitesAcceptCounts{
			Accepted:    summary.Accepted,
			WouldAccept: summary.WouldAccept,
			Skipped:     summary.Skipped,
			Failed:      summary.Failed,
		},
	}
}

// inviteReasonText gives skips a fixed sentence and failures the provider's
// error text, which the adapters already scrub of response bodies.
func inviteReasonText(outcome invitations.Outcome) string {
	switch outcome.Reason {
	case invitations.ReasonNotAllowlisted:
		return "not on the allowlist"
	case invitations.ReasonExpired:
		return "invitation expired; ask the inviter to send a new one"
	case invitations.ReasonAuth,
		invitations.ReasonPermission,
		invitations.ReasonNotFound,
		invitations.ReasonConflict,
		invitations.ReasonRetryable,
		invitations.ReasonCanceled,
		invitations.ReasonError:
		if outcome.Err != nil {
			return outcome.Err.Error()
		}
		return string(outcome.Reason)
	default:
		return ""
	}
}

func newInviteItem(invitation gitprovider.RepositoryInvitation) InviteItem {
	return InviteItem{
		ID:         string(invitation.ID),
		Repository: invitation.FullName(),
		Inviter:    invitation.Inviter.Login,
		Permission: invitation.Permission,
		CreatedAt:  invitation.CreatedAt,
		Expired:    invitation.Expired,
		URL:        invitation.URL,
	}
}

// RenderInvitesListText writes a stable human-readable invitation list.
func RenderInvitesListText(w io.Writer, result InvitesList) error {
	if err := writeInvitesIdentity(w, result.InvitesIdentity); err != nil {
		return err
	}
	if len(result.Invitations) == 0 {
		_, err := fmt.Fprintln(w, "Invitations: none")
		return err
	}
	if _, err := fmt.Fprintln(w, "Invitations:"); err != nil {
		return err
	}
	for _, item := range result.Invitations {
		if _, err := fmt.Fprintf(w, "  - %s\n", item.Repository); err != nil {
			return err
		}
		if err := writeInviteDetails(w, item); err != nil {
			return err
		}
	}
	return nil
}

// RenderInvitesAcceptText writes a stable human-readable accept report.
func RenderInvitesAcceptText(w io.Writer, result InvitesAccept) error {
	if err := writeInvitesIdentity(w, result.InvitesIdentity); err != nil {
		return err
	}
	if err := writeKV(w, "Dry run", fmt.Sprint(result.DryRun)); err != nil {
		return err
	}
	if len(result.Results) == 0 {
		if _, err := fmt.Fprintln(w, "Invitations: none"); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintln(w, "Invitations:"); err != nil {
			return err
		}
		for _, item := range result.Results {
			line := fmt.Sprintf("  - %s: %s", item.Repository, item.Status)
			if item.Reason != "" {
				line += " (" + item.Reason + ")"
			}
			if _, err := fmt.Fprintln(w, line); err != nil {
				return err
			}
			if err := writeInviteDetails(w, item.InviteItem); err != nil {
				return err
			}
		}
	}
	summary := result.Summary
	return writeKV(w, "Summary", fmt.Sprintf("accepted=%d would_accept=%d skipped=%d failed=%d", summary.Accepted, summary.WouldAccept, summary.Skipped, summary.Failed))
}

func writeInvitesIdentity(w io.Writer, identity InvitesIdentity) error {
	if err := writeKV(w, "Profile", identity.Profile); err != nil {
		return err
	}
	if err := writeKV(w, "Credential source", identity.CredentialSource); err != nil {
		return err
	}
	if err := writeKV(w, "Host", identity.Host); err != nil {
		return err
	}
	return writeKV(w, "Login", identity.Login)
}

func writeInviteDetails(w io.Writer, item InviteItem) error {
	if err := writeKV(w, "    Permission", item.Permission); err != nil {
		return err
	}
	if err := writeOptionalKV(w, "    Inviter", item.Inviter); err != nil {
		return err
	}
	if !item.CreatedAt.IsZero() {
		if err := writeKV(w, "    Created", item.CreatedAt.UTC().Format(time.RFC3339)); err != nil {
			return err
		}
	}
	if item.Expired {
		return writeKV(w, "    Expired", "true")
	}
	return nil
}

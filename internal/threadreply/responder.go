package threadreply

import (
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/marker"
)

// Candidate is one cr-authored thread that has an unaddressed human reply.
type Candidate struct {
	Thread  gitprovider.InlineThread
	Request Request
}

// SelectCandidates returns the threads that cr should consider replying to:
// unresolved threads whose first comment cr authored (its own finding) and
// whose latest comment is a human reply that cr has not answered yet.
//
// Threads whose latest comment is cr's own are skipped: cr already had the last
// word, so there is no new reply to respond to. This keeps repeated runs
// idempotent without persisted per-thread state.
func SelectCandidates(pr gitprovider.PR, postingIdentity gitprovider.Identity, threads []gitprovider.InlineThread) []Candidate {
	login := strings.TrimSpace(postingIdentity.Login)
	var candidates []Candidate
	for _, thread := range threads {
		if thread.Resolved || len(thread.Comments) == 0 {
			continue
		}
		first := thread.Comments[0]
		if !authoredByCR(first, login) {
			continue
		}
		last := thread.Comments[len(thread.Comments)-1]
		if commentLogin(last) == login && login != "" {
			// cr posted the latest comment; nothing new to respond to.
			continue
		}
		candidates = append(candidates, Candidate{
			Thread:  thread,
			Request: buildRequest(pr, postingIdentity, login, thread),
		})
	}
	return candidates
}

func buildRequest(pr gitprovider.PR, postingIdentity gitprovider.Identity, login string, thread gitprovider.InlineThread) Request {
	comments := make([]Comment, 0, len(thread.Comments))
	original := ""
	for i, c := range thread.Comments {
		fromCR := authoredByCR(c, login)
		// Strip cr's own markers first, then escape any remaining marker
		// openings the comment text may contain, so the model never sees a
		// live marker yet a single representation is used everywhere. Deriving
		// both Body and OriginalFinding from this one value keeps the first
		// comment consistent between the two fields.
		body := marker.SanitizeModelContent(stripMarkers(c.Body))
		if i == 0 {
			original = body
		}
		comments = append(comments, Comment{
			Author:    displayLogin(c),
			Body:      body,
			FromCR:    fromCR,
			CreatedAt: c.CreatedAt,
		})
	}
	return Request{
		PR:              pr,
		PostingIdentity: postingIdentity,
		Path:            thread.Path,
		Line:            thread.Line,
		OriginalFinding: original,
		Comments:        comments,
	}
}

// authoredByCR reports whether comment c was authored by cr. A comment counts as
// cr's when it matches the posting login and carries a codereview action marker,
// so a human posting from the same account is not mistaken for a finding.
func authoredByCR(c gitprovider.ThreadComment, login string) bool {
	if login == "" {
		return false
	}
	if commentLogin(c) != login {
		return false
	}
	return len(marker.FindActions(c.Body)) > 0
}

func commentLogin(c gitprovider.ThreadComment) string {
	return strings.TrimSpace(c.Author.Login)
}

func displayLogin(c gitprovider.ThreadComment) string {
	if login := commentLogin(c); login != "" {
		return login
	}
	return "unknown"
}

// stripMarkers removes codereview HTML-comment markers so the model sees only
// human-meaningful text.
func stripMarkers(body string) string {
	const openTag = "<!-- codereview:"
	const closeTag = " -->"
	var b strings.Builder
	rest := body
	for {
		start := strings.Index(rest, openTag)
		if start == -1 {
			b.WriteString(rest)
			break
		}
		b.WriteString(rest[:start])
		after := rest[start+len(openTag):]
		end := strings.Index(after, closeTag)
		if end == -1 {
			// Unterminated marker: drop the remainder of the opening token.
			break
		}
		rest = after[end+len(closeTag):]
	}
	return strings.TrimSpace(b.String())
}

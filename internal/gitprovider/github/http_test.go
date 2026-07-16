package github

import "testing"

func TestHTTPStatusErrorSurfacesSafeDetail(t *testing.T) {
	body := []byte(`{"message":"Validation Failed","errors":[{"resource":"PullRequestReview","field":"path","code":"invalid"}],"documentation_url":"https://docs.github.com"}`)
	err := httpStatusError(400, body)
	want := "github: status 400 (PullRequestReview.path.invalid)"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}

	// Non-JSON bodies stay fully redacted — they can echo request content.
	err = httpStatusError(400, []byte("<html>some raw error echoing content</html>"))
	if err == nil || err.Error() != "github: status 400 (response body redacted)" {
		t.Fatalf("non-JSON err = %v, want redacted", err)
	}

	// Free-text message alone is NOT surfaced — it can echo secrets/content.
	err = httpStatusError(401, []byte(`{"message":"bad credentials ghp_canary"}`))
	if err == nil || err.Error() != "github: status 401 (response body redacted)" {
		t.Fatalf("message-only err = %v, want redacted", err)
	}

	// JSON without errors metadata also stays redacted.
	err = httpStatusError(422, []byte(`{"data":"user content"}`))
	if err == nil || err.Error() != "github: status 422 (response body redacted)" {
		t.Fatalf("metadata-free err = %v, want redacted", err)
	}
}

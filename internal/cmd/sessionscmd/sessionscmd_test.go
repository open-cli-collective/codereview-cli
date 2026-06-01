package sessionscmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/cli-common/statedirtest"
	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

func TestSessionsListText(t *testing.T) {
	statedirtest.Hermetic(t)
	insertNamedSession(t, namedSession("daily", "provider-session-2"))
	insertNamedSession(t, namedSession("alpha", "provider-session-1"))
	cmd, out := newTestCommand()

	if err := root.Execute(cmd, []string{"sessions", "list"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	text := out.String()
	if !strings.Contains(text, "Sessions:") || !strings.Contains(text, "  - alpha") || !strings.Contains(text, "  - daily") {
		t.Fatalf("stdout = %q, want named sessions", text)
	}
	if strings.Index(text, "  - alpha") > strings.Index(text, "  - daily") {
		t.Fatalf("stdout = %q, want stable name order", text)
	}
}

func TestSessionsListJSONEmpty(t *testing.T) {
	statedirtest.Hermetic(t)
	cmd, out := newTestCommand()

	if err := root.Execute(cmd, []string{"sessions", "list", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var decoded view.SessionsList
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, out.String())
	}
	if len(decoded.Sessions) != 0 {
		t.Fatalf("sessions = %#v, want empty", decoded.Sessions)
	}
}

func TestSessionsShowJSON(t *testing.T) {
	statedirtest.Hermetic(t)
	insertNamedSession(t, namedSession("daily", "provider-session-2"))
	cmd, out := newTestCommand()

	if err := root.Execute(cmd, []string{"sessions", "show", "daily", "--json"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var decoded view.SessionsShow
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, out.String())
	}
	if decoded.Session.Name != "daily" || decoded.Session.ProviderSessionID != "provider-session-2" {
		t.Fatalf("decoded = %#v, want daily session", decoded)
	}
}

func TestSessionsDelete(t *testing.T) {
	statedirtest.Hermetic(t)
	insertNamedSession(t, namedSession("daily", "provider-session-2"))
	cmd, out := newTestCommand()

	if err := root.Execute(cmd, []string{"sessions", "delete", "daily"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.String(), "Deleted session: daily") {
		t.Fatalf("stdout = %q, want deletion message", out.String())
	}
	store := openTestStore(t)
	if _, err := store.GetNamedSession(context.Background(), "daily"); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("GetNamedSession error = %v, want ErrNotFound", err)
	}
}

func TestSessionsMissingRowsFail(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "show", args: []string{"sessions", "show", "missing"}},
		{name: "delete", args: []string{"sessions", "delete", "missing"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			statedirtest.Hermetic(t)
			cmd, _ := newTestCommand()

			err := root.Execute(cmd, tt.args)
			if err == nil {
				t.Fatal("Execute error = nil, want failure")
			}
			if got := exitcode.FromError(err); got != exitcode.Failure {
				t.Fatalf("exit code = %d, want failure", got)
			}
			if !strings.Contains(err.Error(), "not found") {
				t.Fatalf("error = %v, want not found", err)
			}
		})
	}
}

func newTestCommand() (*cobra.Command, *bytes.Buffer) {
	var out bytes.Buffer
	cmd, opts := root.NewCommandWithOptions(&root.Options{
		Stdin:  strings.NewReader(""),
		Stdout: &out,
		Stderr: &out,
	})
	Register(cmd, opts)
	return cmd, &out
}

func insertNamedSession(t *testing.T, session ledger.NamedSession) {
	t.Helper()
	store := openTestStore(t)
	if err := store.UpsertNamedSession(context.Background(), session); err != nil {
		t.Fatalf("UpsertNamedSession: %v", err)
	}
}

func openTestStore(t *testing.T) *ledger.Store {
	t.Helper()
	layout, err := statepaths.DefaultLayoutEnsured()
	if err != nil {
		t.Fatalf("DefaultLayoutEnsured: %v", err)
	}
	store, err := ledger.Open(context.Background(), layout.LedgerDB())
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close: %v", err)
		}
	})
	return store
}

func namedSession(name, providerSessionID string) ledger.NamedSession {
	return ledger.NamedSession{
		Name:              name,
		Profile:           "home",
		Provider:          "anthropic",
		Adapter:           "fake-llm",
		Model:             "sonnet",
		Host:              "github.com",
		ProviderSessionID: providerSessionID,
		CreatedAt:         time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
		LastUsedAt:        time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
	}
}

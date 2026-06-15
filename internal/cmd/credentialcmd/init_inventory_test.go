package credentialcmd

import (
	"io"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/open-cli-collective/codereview-cli/internal/config"
)

func TestInitInventoryVisibleItemsKeepPendingAndCommandsOrderedDuringFilter(t *testing.T) {
	model := newInitInventoryModel(initInventoryPrompt{
		Title:       "Reviewer entity",
		Description: "Choose who posts reviews.",
		Width:       80,
		Height:      20,
		Rows: []initInventoryRow{
			{ID: "app", Title: "GitHub App reviewer: org-bot", Kind: initInventoryRowKindActive, Selectable: true, Deletable: true},
			{ID: "pat", Title: "PAT reviewer", FilterValue: "default-reviewer", Kind: initInventoryRowKindActive, Selectable: true, Deletable: true},
			{ID: "restore-app", Title: "GitHub App reviewer: old-bot (staged for deletion)", Kind: initInventoryRowKindPending, Restorable: true},
			{ID: "create-pat", Title: "Use a personal access token (PAT) reviewer", Kind: initInventoryRowKindCommand, PrimaryAction: initInventoryActionCommand, Selectable: true},
			{ID: "back", Title: "Back to main menu", Kind: initInventoryRowKindCommand, PrimaryAction: initInventoryActionBack, Selectable: true},
		},
	})

	model.list.SetFilterText("default")

	var got []string
	for _, item := range model.list.VisibleItems() {
		got = append(got, item.(initInventoryItem).row.ID)
	}
	want := []string{"pat", "restore-app", "create-pat", "back"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("visible item ids = %#v, want %#v", got, want)
	}
}

func TestInitInventoryReordersRowsIntoActivePendingAndCommandSections(t *testing.T) {
	model := newInitInventoryModel(initInventoryPrompt{
		Title:  "Reviewer entity",
		Width:  80,
		Height: 20,
		Rows: []initInventoryRow{
			{ID: "back", Title: "Back to main menu", Kind: initInventoryRowKindCommand, PrimaryAction: initInventoryActionBack, Selectable: true},
			{ID: "restore-app", Title: "GitHub App reviewer: old-bot (staged for deletion)", Kind: initInventoryRowKindPending, Restorable: true},
			{ID: "pat", Title: "PAT reviewer: default-reviewer", Kind: initInventoryRowKindActive, Selectable: true, Deletable: true},
		},
	})

	var got []string
	for _, row := range model.rows {
		got = append(got, row.ID)
	}
	want := []string{"pat", "restore-app", "back"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ordered row ids = %#v, want %#v", got, want)
	}
	if model.rows[1].Description != "Pending deletion" {
		t.Fatalf("pending description = %q, want Pending deletion", model.rows[1].Description)
	}
	if model.rows[2].Description != "Actions" {
		t.Fatalf("command description = %q, want Actions", model.rows[2].Description)
	}
}

func TestInitInventoryDeleteKeyStagesDeletionForDeletableRow(t *testing.T) {
	model := newInitInventoryModel(initInventoryPrompt{
		Title:  "LLM runtime",
		Width:  80,
		Height: 20,
		Rows: []initInventoryRow{
			{ID: "claude-cli", Title: "Configured: Claude CLI subscription", Kind: initInventoryRowKindActive, Selectable: true, Deletable: true},
			{ID: "back", Title: "Back to main menu", Kind: initInventoryRowKindCommand, PrimaryAction: initInventoryActionBack, Selectable: true},
		},
	})

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	resultModel := next.(initInventoryModel)
	got := resultModel.Result()
	if got.Action != initInventoryActionStageDelete || got.Row.ID != "claude-cli" {
		t.Fatalf("result = %#v, want stage-delete claude-cli", got)
	}
	if !resultModel.QuitRequested() {
		t.Fatal("quit requested = false, want true after delete action")
	}
}

func TestInitInventoryDeleteKeyIgnoresNonDeletableRow(t *testing.T) {
	model := newInitInventoryModel(initInventoryPrompt{
		Title:  "LLM runtime",
		Width:  80,
		Height: 20,
		Rows: []initInventoryRow{
			{ID: "back", Title: "Back to main menu", Kind: initInventoryRowKindCommand, PrimaryAction: initInventoryActionBack, Selectable: true},
		},
	})

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	resultModel := next.(initInventoryModel)
	if resultModel.Result().Action != initInventoryActionNone {
		t.Fatalf("result = %#v, want no action for non-deletable row", resultModel.Result())
	}
	if resultModel.QuitRequested() {
		t.Fatal("quit requested = true, want false for ignored delete")
	}
}

func TestInitInventoryRestoreKeyRestoresPendingRow(t *testing.T) {
	model := newInitInventoryModel(initInventoryPrompt{
		Title:  "Review profile",
		Width:  80,
		Height: 20,
		Rows: []initInventoryRow{
			{ID: "work", Title: "work (staged for deletion)", Kind: initInventoryRowKindPending, Restorable: true},
			{ID: "back", Title: "Back to main menu", Kind: initInventoryRowKindCommand, PrimaryAction: initInventoryActionBack, Selectable: true},
		},
	})

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	resultModel := next.(initInventoryModel)
	got := resultModel.Result()
	if got.Action != initInventoryActionRestore || got.Row.ID != "work" {
		t.Fatalf("result = %#v, want restore work", got)
	}
}

func TestInitInventoryRestoreKeyIgnoresNonRestorableRow(t *testing.T) {
	model := newInitInventoryModel(initInventoryPrompt{
		Title:  "Review profile",
		Width:  80,
		Height: 20,
		Rows: []initInventoryRow{
			{ID: "pat", Title: "PAT reviewer: default-reviewer", Kind: initInventoryRowKindActive, Selectable: true, Deletable: true},
		},
	})

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	resultModel := next.(initInventoryModel)
	if resultModel.Result().Action != initInventoryActionNone {
		t.Fatalf("result = %#v, want no action for non-restorable row", resultModel.Result())
	}
	if resultModel.QuitRequested() {
		t.Fatal("quit requested = true, want false for ignored restore")
	}
}

func TestInitInventoryFilterAppliedStillAllowsEnterAndRestore(t *testing.T) {
	model := newInitInventoryModel(initInventoryPrompt{
		Title:  "Reviewer entity",
		Width:  80,
		Height: 20,
		Rows: []initInventoryRow{
			{ID: "pat", Title: "PAT reviewer: default-reviewer", Kind: initInventoryRowKindActive, Selectable: true, Deletable: true},
			{ID: "restore-app", Title: "GitHub App reviewer: old-bot (staged for deletion)", Kind: initInventoryRowKindPending, Restorable: true},
			{ID: "back", Title: "Back to main menu", Kind: initInventoryRowKindCommand, PrimaryAction: initInventoryActionBack, Selectable: true},
		},
	})

	model.list.SetFilterText("default")
	enterNext, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	enterResult := enterNext.(initInventoryModel).Result()
	if enterResult.Action != initInventoryActionEdit || enterResult.Row.ID != "pat" {
		t.Fatalf("enter result after filter = %#v, want edit pat", enterResult)
	}

	model.list.SetFilterText("default")
	model.list.Select(1)
	restoreNext, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	restoreResult := restoreNext.(initInventoryModel).Result()
	if restoreResult.Action != initInventoryActionRestore || restoreResult.Row.ID != "restore-app" {
		t.Fatalf("restore result after filter = %#v, want restore restore-app", restoreResult)
	}
}

func TestInitInventoryEnterSelectsActiveAndCommandRows(t *testing.T) {
	model := newInitInventoryModel(initInventoryPrompt{
		Title:  "Reviewer entity",
		Width:  80,
		Height: 20,
		Rows: []initInventoryRow{
			{ID: "pat", Title: "PAT reviewer: default-reviewer", Kind: initInventoryRowKindActive, Selectable: true, Deletable: true},
			{ID: "create-pat", Title: "Use a personal access token (PAT) reviewer", Kind: initInventoryRowKindCommand, PrimaryAction: initInventoryActionCommand, Selectable: true},
			{ID: "back", Title: "Back to main menu", Kind: initInventoryRowKindCommand, PrimaryAction: initInventoryActionBack, Selectable: true},
		},
	})

	activeNext, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	activeResult := activeNext.(initInventoryModel).Result()
	if activeResult.Action != initInventoryActionEdit || activeResult.Row.ID != "pat" {
		t.Fatalf("active result = %#v, want edit pat", activeResult)
	}

	model.list.Select(1)
	createNext, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	createResult := createNext.(initInventoryModel).Result()
	if createResult.Action != initInventoryActionCommand || createResult.Row.ID != "create-pat" {
		t.Fatalf("create result = %#v, want command create-pat", createResult)
	}

	model.list.Select(2)
	backNext, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	backResult := backNext.(initInventoryModel).Result()
	if backResult.Action != initInventoryActionBack || backResult.Row.ID != "back" {
		t.Fatalf("back result = %#v, want back command", backResult)
	}
}

func TestInitInventoryEnterIgnoresNonSelectableRow(t *testing.T) {
	model := newInitInventoryModel(initInventoryPrompt{
		Title:  "Reviewer entity",
		Width:  80,
		Height: 20,
		Rows: []initInventoryRow{
			{ID: "status", Title: "Configured reviewer summary", Kind: initInventoryRowKindActive},
		},
	})

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	resultModel := next.(initInventoryModel)
	if resultModel.Result().Action != initInventoryActionNone {
		t.Fatalf("result = %#v, want no action for non-selectable row", resultModel.Result())
	}
	if resultModel.QuitRequested() {
		t.Fatal("quit requested = true, want false for ignored select")
	}
}

func TestInitInventoryBackKeyReturnsBackAction(t *testing.T) {
	model := newInitInventoryModel(initInventoryPrompt{
		Title:  "Reviewer entity",
		Width:  80,
		Height: 20,
		Rows: []initInventoryRow{
			{ID: "pat", Title: "PAT reviewer: default-reviewer", Kind: initInventoryRowKindActive, Selectable: true, Deletable: true},
		},
	})

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	resultModel := next.(initInventoryModel)
	if resultModel.Result().Action != initInventoryActionBack {
		t.Fatalf("result = %#v, want back action", resultModel.Result())
	}
}

func TestInitInventoryDeterministicRunnerReturnsCommandAction(t *testing.T) {
	result, err := runInitInventory(initInventoryPrompt{
		Title:  "Reviewer entity",
		Width:  80,
		Height: 20,
		Mode:   initInventoryModeDeterministic,
		Messages: []tea.Msg{
			tea.KeyMsg{Type: tea.KeyDown},
			tea.KeyMsg{Type: tea.KeyEnter},
			tea.KeyMsg{Type: tea.KeyUp},
		},
		Rows: []initInventoryRow{
			{ID: "pat", Title: "PAT reviewer: default-reviewer", Kind: initInventoryRowKindActive, Selectable: true, Deletable: true},
			{ID: "back", Title: "Back to main menu", Kind: initInventoryRowKindCommand, PrimaryAction: initInventoryActionBack, Selectable: true},
		},
	}, strings.NewReader(""), io.Discard)
	if err != nil {
		t.Fatalf("runInitInventory: %v", err)
	}
	if result.Action != initInventoryActionBack || result.Row.ID != "back" {
		t.Fatalf("result = %#v, want deterministic back command", result)
	}
}

func TestInitReviewerEntityInventoryRowsSetExpectedCapabilities(t *testing.T) {
	rows := initReviewerEntityInventoryRows(initPromptContext{
		ExistingProfileName: "work",
		ReviewerEntities: map[string]initReviewerEntityDraft{
			"reviewer-github-app": {
				Name:          "reviewer-github-app",
				Kind:          initReviewerEntityKindGitHubApp,
				AuthMode:      config.GitAuthModeGitHubApp,
				CredentialRef: "codereview/open-cli-collective-rianjs-bot",
				DisplayName:   "OC Collective bot",
			},
		},
		PendingReviewerEntityDeletes: map[string]initPendingReviewerEntityDelete{
			"reviewer-pat": {EntityName: "reviewer-pat"},
		},
	})

	if len(rows) != 6 {
		t.Fatalf("len(rows) = %d, want 6", len(rows))
	}
	if got := rows[0]; got.Kind != initInventoryRowKindActive || !got.Selectable || !got.Deletable || got.Restorable || got.PrimaryAction != initInventoryActionNone {
		t.Fatalf("active row = %#v, want selectable+deletable active reviewer entity", got)
	}
	if got := rows[1]; got.Kind != initInventoryRowKindPending || got.Selectable || got.Deletable || !got.Restorable || got.PrimaryAction != initInventoryActionNone {
		t.Fatalf("pending row = %#v, want restorable pending reviewer entity", got)
	}
	if got := rows[2]; got.Kind != initInventoryRowKindCommand || !got.Selectable || got.PrimaryAction != initInventoryActionCommand {
		t.Fatalf("fallback row = %#v, want selectable command fallback", got)
	}
	if got := rows[3]; got.Kind != initInventoryRowKindCommand || !got.Selectable || got.PrimaryAction != initInventoryActionCommand {
		t.Fatalf("pat row = %#v, want selectable command PAT template", got)
	}
	if got := rows[4]; got.Kind != initInventoryRowKindCommand || !got.Selectable || got.PrimaryAction != initInventoryActionCommand {
		t.Fatalf("github app row = %#v, want selectable command GitHub App template", got)
	}
	if got := rows[5]; got.Kind != initInventoryRowKindCommand || !got.Selectable || got.PrimaryAction != initInventoryActionBack {
		t.Fatalf("back row = %#v, want selectable Back command", got)
	}
}

func TestInitLLMRuntimeInventoryRowsSetExpectedCapabilities(t *testing.T) {
	rows := initLLMRuntimeInventoryRows(initPromptContext{
		LLMRuntimes: map[string]initLLMRuntimeDraft{
			"claude-cli": {
				Name:     "claude-cli",
				Preset:   initLLMRuntimePresetClaudeCLISubscription,
				Provider: config.LLMProviderAnthropic,
				Auth:     config.LLMAuthSubscription,
				Adapter:  config.LLMAdapterClaudeCLI,
			},
		},
		PendingLLMRuntimeDeletes: map[string]initPendingLLMRuntimeDelete{
			"codex-cli": {RuntimeName: "codex-cli"},
		},
	})

	if len(rows) != 9 {
		t.Fatalf("len(rows) = %d, want 9", len(rows))
	}
	if got := rows[0]; got.Kind != initInventoryRowKindActive || !got.Selectable || !got.Deletable || got.Restorable || got.PrimaryAction != initInventoryActionNone {
		t.Fatalf("active row = %#v, want selectable+deletable active llm runtime", got)
	}
	if got := rows[1]; got.Kind != initInventoryRowKindPending || got.Selectable || got.Deletable || !got.Restorable || got.PrimaryAction != initInventoryActionNone {
		t.Fatalf("pending row = %#v, want restorable pending llm runtime", got)
	}
	for i := 2; i <= 7; i++ {
		if got := rows[i]; got.Kind != initInventoryRowKindCommand || !got.Selectable || got.PrimaryAction != initInventoryActionCommand {
			t.Fatalf("command row %d = %#v, want selectable command runtime row", i, got)
		}
	}
	if got := rows[8]; got.Kind != initInventoryRowKindCommand || !got.Selectable || got.PrimaryAction != initInventoryActionBack {
		t.Fatalf("back row = %#v, want selectable Back command", got)
	}
}

func TestInitInventoryViewShowsHelpBindings(t *testing.T) {
	model := newInitInventoryModel(initInventoryPrompt{
		Title:       "Reviewer entity",
		Description: "Choose who posts reviews.",
		Width:       80,
		Height:      20,
		Rows: []initInventoryRow{
			{ID: "pat", Title: "PAT reviewer: default-reviewer", Kind: initInventoryRowKindActive, Selectable: true, Deletable: true},
			{ID: "restore-pat", Title: "PAT reviewer: old-reviewer (staged for deletion)", Kind: initInventoryRowKindPending, Restorable: true},
			{ID: "back", Title: "Back to main menu", Kind: initInventoryRowKindCommand, PrimaryAction: initInventoryActionBack, Selectable: true},
		},
	})

	out := model.View()
	for _, want := range []string{
		"Choose who posts reviews.",
		"enter",
		"select",
		"d",
		"delete",
		"r",
		"restore",
		"esc",
		"back",
		"Pending deletion",
		"Actions",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("view = %q, want %q", out, want)
		}
	}
}

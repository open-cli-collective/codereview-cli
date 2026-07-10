package credentialcmd

import (
	"fmt"
	"io"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"

	"github.com/open-cli-collective/codereview-cli/internal/config"
)

type initLLMRuntimeEditorRunner func(initLinearEditor, io.Reader, io.Writer) (initLinearEditorModel, error)

const (
	initLLMRuntimeFieldSelection       initLinearFieldID = "llm_runtime_selection"
	initLLMRuntimeFieldProvider        initLinearFieldID = "llm_runtime_provider"
	initLLMRuntimeFieldAuth            initLinearFieldID = "llm_runtime_auth"
	initLLMRuntimeFieldAdapter         initLinearFieldID = "llm_runtime_adapter"
	initLLMRuntimeFieldCredentialStore initLinearFieldID = "llm_runtime_credential_store" // #nosec G101 -- field ID, not a secret.
	initLLMRuntimeFieldCredentialName  initLinearFieldID = "llm_runtime_credential_name"  // #nosec G101 -- field ID, not a secret.
	initLLMRuntimeFieldReplacement     initLinearFieldID = "llm_runtime_replacement"
	initLLMRuntimeFieldAction          initLinearFieldID = "llm_runtime_action"
)

const (
	initLLMRuntimeActionDelete  = "delete"
	initLLMRuntimeActionRestore = "restore"
)

const initLLMRuntimeRestoreSelectionPrefix = "__restore_llm_runtime__:"

func (p huhInitLLMRuntimePrompter) editLLMRuntimeLinear(prompt initLLMRuntimePrompt) (initDraft, error) {
	seed := seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.ExistingProfile)
	editor := initLLMRuntimeLinearEditor(prompt.Context, seed, p.runtimeAvailabilityNote)
	model, err := p.runLLMRuntimeEditor(editor)
	if err != nil {
		return initDraft{}, err
	}
	switch model.resultAction {
	case initDetailActionEdit:
		return initLLMRuntimeDraftFromLinearDocument(seed, prompt.Context, model.document), nil
	case initLLMRuntimeActionDelete:
		draft := initLLMRuntimeDraftForDeleteFromLinearDocument(seed, prompt.Context, model.document)
		draft.Action = initDraftActionDeleteLLMRuntime
		draft.ActionTarget = initLLMRuntimeSelectionValue(model.document)
		return draft, nil
	case initLLMRuntimeActionRestore:
		runtimeName, _ := initLLMRuntimeRestoreSelectionName(initLLMRuntimeSelectionValue(model.document))
		return initDraft{
			Action:       initDraftActionUndoDeleteLLMRuntime,
			ActionTarget: runtimeName,
		}, nil
	default:
		return initDraft{}, errInitNavigateBack
	}
}

func (p huhInitLLMRuntimePrompter) editLLMRuntimeDetailsLinear(seed initDraft) (initDraft, bool, error) {
	editor := initLLMRuntimeDetailsEditor(seed, p.runtimeAvailabilityNote)
	model, err := p.runLLMRuntimeEditor(editor)
	if err != nil {
		return initDraft{}, false, err
	}
	switch model.resultAction {
	case initDetailActionEdit:
		return initLLMRuntimeDraftFromDocument(seed, model.document), false, nil
	default:
		return initDraft{}, true, nil
	}
}

func (p huhInitLLMRuntimePrompter) runLLMRuntimeEditor(editor initLinearEditor) (initLinearEditorModel, error) {
	if p.editorRunner != nil {
		return p.editorRunner(editor, p.stdin, p.stderr)
	}
	program := tea.NewProgram(newInitLinearEditorModel(editor, 100, 28), tea.WithInput(p.stdin), tea.WithOutput(p.stderr))
	finalModel, err := program.Run()
	if err != nil {
		return initLinearEditorModel{}, err
	}
	model, ok := finalModel.(initLinearEditorModel)
	if !ok {
		return initLinearEditorModel{}, fmt.Errorf("LLM runtime editor returned %T", finalModel)
	}
	return model, nil
}

func initLLMRuntimeDetailsEditor(seed initDraft, availabilityNote func(initLLMRuntimePreset) string) initLinearEditor {
	runtime := initLLMRuntimeDraftFromSeedDraft(seed)
	description := initLLMRuntimeSelectionDescription(runtime, availabilityNote(runtime.Preset))
	var document initLinearDocument
	document.addSection("LLM runtime", description)
	document.addEditableSelect(initLLMRuntimeFieldProvider, "LLM provider", "", []huh.Option[string]{
		huh.NewOption("Anthropic", string(config.LLMProviderAnthropic)),
		huh.NewOption("OpenAI", string(config.LLMProviderOpenAI)),
		huh.NewOption("Pi", string(config.LLMProviderPi)),
	}, seed.LLMProvider)
	document.addEditableSelect(initLLMRuntimeFieldAuth, "LLM auth mode", "", []huh.Option[string]{
		huh.NewOption("Subscription", string(config.LLMAuthSubscription)),
		huh.NewOption("API key", string(config.LLMAuthAPIKey)),
	}, seed.LLMAuth)
	document.addEditableSelect(initLLMRuntimeFieldAdapter, "LLM adapter", "", initLLMRuntimeAdapterOptions(), seed.LLMAdapter)
	document.addEditableSelect(initLLMRuntimeFieldAction, "Runtime detail action", "", []huh.Option[string]{
		huh.NewOption("Stage these runtime details", initDetailActionEdit),
		huh.NewOption("Back without staging", initDetailActionBack),
	}, initDetailActionEdit)
	return initLinearEditor{
		Document: document,
		OnEnter: func(model *initLinearEditorModel) (bool, tea.Cmd) {
			if model.focused < 0 || model.focused >= len(model.document) {
				return false, nil
			}
			if model.document[model.focused].ID != initLLMRuntimeFieldAction {
				return false, nil
			}
			switch model.document.selectedValue(initLLMRuntimeFieldAction) {
			case initDetailActionBack:
				model.resultAction = initDetailActionBack
				return true, tea.Quit
			case initDetailActionEdit:
				model.resultAction = initDetailActionEdit
				return true, tea.Quit
			default:
				return true, nil
			}
		},
	}
}

func initLLMRuntimeLinearEditor(ctx initPromptContext, seed initDraft, availabilityNote func(initLLMRuntimePreset) string) initLinearEditor {
	selectionOptions := initLLMRuntimeLinearSelectionOptions(ctx)
	selection := initLLMRuntimeDefaultSelection(ctx, seed, selectionOptions)
	replacementOptions := initLLMRuntimeReplacementOptions(ctx, selection)

	var document initLinearDocument
	document.addSection("LLM runtime", "Choose how reviewer agents run. Configured runtimes can be reused by review profiles; templates seed common runtime shapes.")
	document.addEditableSelect(initLLMRuntimeFieldSelection, "Runtime", "", selectionOptions, selection)
	document.addSection("Runtime details", "")
	document.addEditableSelect(initLLMRuntimeFieldProvider, "LLM provider", "", []huh.Option[string]{
		huh.NewOption("Anthropic", string(config.LLMProviderAnthropic)),
		huh.NewOption("OpenAI", string(config.LLMProviderOpenAI)),
		huh.NewOption("Pi", string(config.LLMProviderPi)),
	}, seed.LLMProvider)
	document.addEditableSelect(initLLMRuntimeFieldAuth, "LLM auth mode", "", []huh.Option[string]{
		huh.NewOption("Subscription", string(config.LLMAuthSubscription)),
		huh.NewOption("API key", string(config.LLMAuthAPIKey)),
	}, seed.LLMAuth)
	document.addEditableSelect(initLLMRuntimeFieldAdapter, "LLM adapter", "", initLLMRuntimeAdapterOptions(), seed.LLMAdapter)
	document.addEditableSelect(
		initLLMRuntimeFieldCredentialStore,
		"LLM credential store",
		"Where this runtime's API key is stored.",
		initCredentialStoreOptions(ctx.ExistingConfig),
		initCredentialStoreDraftValue(seed.LLMCredentialStore),
		initLinearFieldOptions{Hidden: true},
	)
	document.addEditableInput(
		initLLMRuntimeFieldCredentialName,
		"LLM credential name",
		"Full credential name under the selected store.",
		"",
		validateRequiredCredentialRef,
		initLinearFieldOptions{Hidden: true},
	)
	document.addEditableSelect(initLLMRuntimeFieldReplacement, "Replacement LLM runtime", "Choose the runtime that should replace this deleted runtime for every affected profile.", replacementOptions, normalizeInitStringSelectionValue("", replacementOptions), initLinearFieldOptions{Hidden: true})
	document.addEditableSelect(initLLMRuntimeFieldAction, "Runtime action", "", initLLMRuntimeActionOptions(ctx, selection, false), initDetailActionEdit)
	editor := initLinearEditor{
		Document: document,
		OnFieldChange: func(model *initLinearEditorModel, index int) {
			if index < 0 || index >= len(model.document) {
				return
			}
			id := model.document[index].ID
			if id == initLLMRuntimeFieldSelection && model.document.selectedValue(initLLMRuntimeFieldAction) == initLLMRuntimeActionDelete {
				model.selectFieldValue(initLLMRuntimeFieldAction, initDetailActionEdit)
			}
			if id == initLLMRuntimeFieldSelection ||
				id == initLLMRuntimeFieldAction ||
				id == initLLMRuntimeFieldReplacement {
				initLLMRuntimeSyncLinearFields(model, ctx, seed, availabilityNote, true)
				return
			}
			if id == initLLMRuntimeFieldProvider ||
				id == initLLMRuntimeFieldAuth ||
				id == initLLMRuntimeFieldAdapter {
				initLLMRuntimeSyncLinearFields(model, ctx, seed, availabilityNote, false)
			}
		},
		OnDelete: func(model *initLinearEditorModel, index int) (bool, tea.Cmd) {
			if index < 0 || index >= len(model.document) {
				return false, nil
			}
			if model.document[index].ID != initLLMRuntimeFieldSelection {
				return false, nil
			}
			selection := model.document.selectedValue(initLLMRuntimeFieldSelection)
			if _, configured := ctx.LLMRuntimes[selection]; !configured {
				return false, nil
			}
			actionIndex := model.document.fieldIndexByID(initLLMRuntimeFieldAction)
			if actionIndex >= 0 {
				model.document[actionIndex].Options = initLinearOptionsFromHuh(initLLMRuntimeActionOptions(ctx, selection, true), initLLMRuntimeActionDelete)
			}
			initLLMRuntimeSyncLinearFields(model, ctx, seed, availabilityNote, true)
			replacementIndex := model.document.fieldIndexByID(initLLMRuntimeFieldReplacement)
			if replacementIndex >= 0 && !model.document[replacementIndex].Hidden {
				model.focused = replacementIndex
			}
			return true, nil
		},
		OnEnter: func(model *initLinearEditorModel) (bool, tea.Cmd) {
			if model.focused < 0 || model.focused >= len(model.document) {
				return false, nil
			}
			if model.document[model.focused].ID != initLLMRuntimeFieldAction {
				return false, nil
			}
			action := model.document.selectedValue(initLLMRuntimeFieldAction)
			switch action {
			case initDetailActionBack:
				model.resultAction = initDetailActionBack
				return true, tea.Quit
			case initDetailActionEdit:
				if err := validateLLMRuntimeLinearDocument(model.document); err != nil {
					model.document[model.focused].Error = err.Error()
					model.relayout()
					model.ensureFocusedVisible()
					return true, nil
				}
				model.resultAction = initDetailActionEdit
				return true, tea.Quit
			case initLLMRuntimeActionDelete:
				replacement := model.document.selectedValue(initLLMRuntimeFieldReplacement)
				if replacement == "" ||
					replacement == model.document.selectedValue(initLLMRuntimeFieldSelection) ||
					initLLMRuntimeDeleteReplacementMatchesDeleted(seed, ctx, model.document) {
					model.document[model.focused].Error = "choose a replacement LLM runtime before staging deletion"
					model.relayout()
					model.ensureFocusedVisible()
					return true, nil
				}
				model.resultAction = initLLMRuntimeActionDelete
				return true, tea.Quit
			case initLLMRuntimeActionRestore:
				model.resultAction = initLLMRuntimeActionRestore
				return true, tea.Quit
			default:
				return true, nil
			}
		},
	}
	model := newInitLinearEditorModel(editor, 100, 28)
	initLLMRuntimeSyncLinearFields(&model, ctx, seed, availabilityNote, true)
	editor.Document = model.document
	return editor
}

func initLLMRuntimeAdapterOptions() []huh.Option[string] {
	specs := config.LLMRuntimeSpecs()
	options := make([]huh.Option[string], 0, len(specs))
	for _, spec := range specs {
		options = append(options, huh.NewOption(spec.DisplayName, string(spec.Adapter)))
	}
	return options
}

func initLLMRuntimeDraftFromDocument(seed initDraft, document initLinearDocument) initDraft {
	draft := seed
	selection := document.selectedValue(initLLMRuntimeFieldSelection)
	if selection == "" {
		selection = document.selectedValue(initLLMRuntimeFieldReplacement)
	}
	if document.selectedValue(initLLMRuntimeFieldAction) == initLLMRuntimeActionDelete {
		selection = document.selectedValue(initLLMRuntimeFieldReplacement)
	}
	if selection != "" && selection != initCustomLLMRuntimeSelection {
		applyLLMRuntimeSelection(&draft, selection)
	}
	draft.LLMProvider = document.selectedValue(initLLMRuntimeFieldProvider)
	draft.LLMAuth = document.selectedValue(initLLMRuntimeFieldAuth)
	draft.LLMAdapter = document.selectedValue(initLLMRuntimeFieldAdapter)
	resolvedRuntimePreset := string(initLLMRuntimeDraftFromSeedDraft(draft).Preset)
	applyLLMRuntimeSelection(&draft, resolvedRuntimePreset)
	return draft
}

func initLLMRuntimeDraftFromLinearDocument(seed initDraft, ctx initPromptContext, document initLinearDocument) initDraft {
	draft := seed
	selection := document.selectedValue(initLLMRuntimeFieldSelection)
	if document.selectedValue(initLLMRuntimeFieldAction) == initLLMRuntimeActionDelete {
		selection = document.selectedValue(initLLMRuntimeFieldReplacement)
	}
	if selection != "" && selection != initCustomLLMRuntimeSelection {
		applyLLMRuntimeInventorySelection(&draft, selection, ctx.LLMRuntimes)
		resolvedRuntimePreset := string(initLLMRuntimeDraftFromSeedDraft(draft).Preset)
		applyLLMRuntimeSelection(&draft, resolvedRuntimePreset)
	}
	draft.LLMProvider = document.selectedValue(initLLMRuntimeFieldProvider)
	draft.LLMAuth = document.selectedValue(initLLMRuntimeFieldAuth)
	draft.LLMAdapter = document.selectedValue(initLLMRuntimeFieldAdapter)
	resolvedRuntimePreset := string(initLLMRuntimeDraftFromSeedDraft(draft).Preset)
	applyLLMRuntimeSelection(&draft, resolvedRuntimePreset)
	if config.LLMAuth(draft.LLMAuth) == config.LLMAuthAPIKey {
		draft.LLMCredentialStore = initCredentialStoreDraftValue(document.selectedValue(initLLMRuntimeFieldCredentialStore))
		draft.LLMCredentialRef = strings.TrimSpace(document.fieldValue(initLLMRuntimeFieldCredentialName))
	} else {
		draft.LLMCredentialStore = initCredentialStoreDefaultID()
		draft.LLMCredentialRef = ""
	}
	return draft
}

func initLLMRuntimeLinearSelectionOptions(ctx initPromptContext) []huh.Option[string] {
	options := initLLMRuntimeOptions(ctx.LLMRuntimes)
	pendingNames := make([]string, 0, len(ctx.PendingLLMRuntimeDeletes))
	for name := range ctx.PendingLLMRuntimeDeletes {
		pendingNames = append(pendingNames, name)
	}
	sort.Strings(pendingNames)
	for _, name := range pendingNames {
		options = append(options, huh.NewOption(llmRuntimeDeletePendingLabel(name), initLLMRuntimeRestoreSelectionPrefix+name))
	}
	return dedupeInitStringOptions(options)
}

func initLLMRuntimeDefaultSelection(ctx initPromptContext, seed initDraft, options []huh.Option[string]) string {
	if selected := strings.TrimSpace(ctx.ProfileLLMRuntimes[ctx.ExistingProfileName]); selected != "" {
		return normalizeInitStringSelectionValue(selected, options)
	}
	currentRuntime := initLLMRuntimeDraftFromSeedDraft(seed)
	for name, runtime := range ctx.LLMRuntimes {
		if runtime.identityKey() == currentRuntime.identityKey() {
			return normalizeInitStringSelectionValue(name, options)
		}
	}
	return normalizeInitStringSelectionValue(string(currentRuntime.Preset), options)
}

func initLLMRuntimeSelectionValue(document initLinearDocument) string {
	return document.selectedValue(initLLMRuntimeFieldSelection)
}

func initLLMRuntimeRestoreSelectionName(selection string) (string, bool) {
	if !strings.HasPrefix(selection, initLLMRuntimeRestoreSelectionPrefix) {
		return "", false
	}
	return strings.TrimPrefix(selection, initLLMRuntimeRestoreSelectionPrefix), true
}

func initLLMRuntimeActionOptions(_ initPromptContext, selection string, includeDelete bool) []huh.Option[string] {
	if _, ok := initLLMRuntimeRestoreSelectionName(selection); ok {
		return []huh.Option[string]{
			huh.NewOption("Back without staging", initDetailActionBack),
		}
	}
	options := []huh.Option[string]{
		huh.NewOption("Stage these runtime details", initDetailActionEdit),
	}
	if includeDelete {
		options = append(options, huh.NewOption("Stage runtime deletion", initLLMRuntimeActionDelete))
	}
	options = append(options, huh.NewOption("Back without staging", initDetailActionBack))
	return options
}

func initLLMRuntimeReplacementOptions(ctx initPromptContext, deletedRuntimeName string) []huh.Option[string] {
	options := make([]huh.Option[string], 0, len(ctx.LLMRuntimes)+6)
	deletedKey, hasDeletedKey := initLLMRuntimeSelectionIdentityKey(deletedRuntimeName, ctx.LLMRuntimes)
	for _, option := range initLLMRuntimeOptions(ctx.LLMRuntimes) {
		if option.Value == deletedRuntimeName {
			continue
		}
		if hasDeletedKey {
			candidateKey, ok := initLLMRuntimeSelectionIdentityKey(option.Value, ctx.LLMRuntimes)
			if ok && candidateKey == deletedKey {
				continue
			}
		}
		options = append(options, option)
	}
	return dedupeInitStringOptions(options)
}

func initLLMRuntimeSelectionIdentityKey(selection string, runtimes map[string]initLLMRuntimeDraft) (string, bool) {
	if runtime, ok := runtimes[selection]; ok {
		return runtime.identityKey(), true
	}
	if selection == initCustomLLMRuntimeSelection {
		return "", false
	}
	var draft initDraft
	applyLLMRuntimeSelection(&draft, selection)
	runtime := initLLMRuntimeDraftFromSeedDraft(draft)
	if runtime.Provider == "" || runtime.Auth == "" || runtime.Adapter == "" {
		return "", false
	}
	return runtime.identityKey(), true
}

func initLLMRuntimeDeleteReplacementMatchesDeleted(seed initDraft, ctx initPromptContext, document initLinearDocument) bool {
	deletedKey, ok := initLLMRuntimeSelectionIdentityKey(document.selectedValue(initLLMRuntimeFieldSelection), ctx.LLMRuntimes)
	if !ok {
		return false
	}
	replacementDraft := initLLMRuntimeDraftForDeleteFromLinearDocument(seed, ctx, document)
	return initLLMRuntimeDraftFromSeedDraft(replacementDraft).identityKey() == deletedKey
}

func initLLMRuntimeSyncLinearFields(model *initLinearEditorModel, ctx initPromptContext, seed initDraft, availabilityNote func(initLLMRuntimePreset) string, resetDetails bool) {
	selection := model.document.selectedValue(initLLMRuntimeFieldSelection)
	initLLMRuntimeSetSelectionOptions(model, ctx, selection)
	actionIndex := model.document.fieldIndexByID(initLLMRuntimeFieldAction)
	if actionIndex >= 0 {
		selectedAction := model.document.selectedValue(initLLMRuntimeFieldAction)
		model.document[actionIndex].Options = initLinearOptionsFromHuh(initLLMRuntimeActionOptions(ctx, selection, selectedAction == initLLMRuntimeActionDelete), selectedAction)
		if model.document.selectedValue(initLLMRuntimeFieldAction) == "" {
			model.document[actionIndex].Options = initLinearOptionsFromHuh(initLLMRuntimeActionOptions(ctx, selection, false), initDetailActionEdit)
		}
	}
	action := model.document.selectedValue(initLLMRuntimeFieldAction)
	replacementVisible := action == initLLMRuntimeActionDelete
	model.setFieldHidden(initLLMRuntimeFieldReplacement, !replacementVisible)
	replacementOptions := initLLMRuntimeReplacementOptions(ctx, selection)
	replacementIndex := model.document.fieldIndexByID(initLLMRuntimeFieldReplacement)
	if replacementIndex >= 0 {
		selectedReplacement := model.document.selectedValue(initLLMRuntimeFieldReplacement)
		model.document[replacementIndex].Options = initLinearOptionsFromHuh(replacementOptions, selectedReplacement)
		if model.document.selectedValue(initLLMRuntimeFieldReplacement) == "" {
			model.document[replacementIndex].Options = initLinearOptionsFromHuh(replacementOptions, normalizeInitStringSelectionValue("", replacementOptions))
		}
	}

	detailSelection := selection
	if replacementVisible {
		detailSelection = model.document.selectedValue(initLLMRuntimeFieldReplacement)
	}
	draft := seed
	if detailSelection != "" && detailSelection != initCustomLLMRuntimeSelection {
		applyLLMRuntimeInventorySelection(&draft, detailSelection, ctx.LLMRuntimes)
		resolvedRuntimePreset := string(initLLMRuntimeDraftFromSeedDraft(draft).Preset)
		applyLLMRuntimeSelection(&draft, resolvedRuntimePreset)
	}
	if resetDetails {
		model.selectFieldValue(initLLMRuntimeFieldProvider, draft.LLMProvider)
		model.selectFieldValue(initLLMRuntimeFieldAuth, draft.LLMAuth)
		model.selectFieldValue(initLLMRuntimeFieldAdapter, draft.LLMAdapter)
	}
	initLLMRuntimeSyncCredentialFields(model, ctx, draft, detailSelection, resetDetails)
	initLLMRuntimeSetDetailsDescription(model, draft, availabilityNote)
}

func initLLMRuntimeSyncCredentialFields(model *initLinearEditorModel, ctx initPromptContext, draft initDraft, selection string, reset bool) {
	auth := config.LLMAuth(model.document.selectedValue(initLLMRuntimeFieldAuth))
	if auth == "" {
		auth = config.LLMAuth(draft.LLMAuth)
	}
	needsCredential := auth == config.LLMAuthAPIKey
	model.setFieldHidden(initLLMRuntimeFieldCredentialStore, !needsCredential)
	model.setFieldHidden(initLLMRuntimeFieldCredentialName, !needsCredential)
	if !needsCredential {
		return
	}
	if reset || strings.TrimSpace(model.document.fieldValue(initLLMRuntimeFieldCredentialName)) == "" {
		store := initCredentialStoreDraftValue(draft.LLMCredentialStore)
		if store == "" {
			store = initCredentialStoreDefaultID()
		}
		model.selectFieldValue(initLLMRuntimeFieldCredentialStore, store)
		name := strings.TrimSpace(draft.LLMCredentialRef)
		if name == "" {
			name = initLLMRuntimeDefaultCredentialName(ctx, draft, selection)
		}
		model.setFieldValue(initLLMRuntimeFieldCredentialName, name)
	}
	model.validateField(model.document.fieldIndexByID(initLLMRuntimeFieldCredentialName))
}

func initLLMRuntimeDefaultCredentialName(ctx initPromptContext, draft initDraft, selection string) string {
	runtime := initLLMRuntimeDraftFromSeedDraft(draft)
	nameSeed := runtime.suggestedName()
	if _, configured := ctx.LLMRuntimes[selection]; configured {
		nameSeed = strings.TrimSpace(selection)
	}
	ref, err := initCredentialRefFromSeed(nameSeed)
	if err != nil {
		return ""
	}
	return ref
}

func validateLLMRuntimeLinearDocument(document initLinearDocument) error {
	if config.LLMAuth(document.selectedValue(initLLMRuntimeFieldAuth)) != config.LLMAuthAPIKey {
		return nil
	}
	if strings.TrimSpace(document.selectedValue(initLLMRuntimeFieldCredentialStore)) == "" {
		return fmt.Errorf("LLM credential store is required")
	}
	return validateRequiredCredentialRef(document.fieldValue(initLLMRuntimeFieldCredentialName))
}

func initLLMRuntimeDraftForDeleteFromLinearDocument(seed initDraft, ctx initPromptContext, document initLinearDocument) initDraft {
	draft := seed
	replacement := document.selectedValue(initLLMRuntimeFieldReplacement)
	if replacement != "" && replacement != initCustomLLMRuntimeSelection {
		applyLLMRuntimeInventorySelection(&draft, replacement, ctx.LLMRuntimes)
		resolvedRuntimePreset := string(initLLMRuntimeDraftFromSeedDraft(draft).Preset)
		applyLLMRuntimeSelection(&draft, resolvedRuntimePreset)
		return draft
	}
	draft.LLMProvider = document.selectedValue(initLLMRuntimeFieldProvider)
	draft.LLMAuth = document.selectedValue(initLLMRuntimeFieldAuth)
	draft.LLMAdapter = document.selectedValue(initLLMRuntimeFieldAdapter)
	resolvedRuntimePreset := string(initLLMRuntimeDraftFromSeedDraft(draft).Preset)
	applyLLMRuntimeSelection(&draft, resolvedRuntimePreset)
	if config.LLMAuth(draft.LLMAuth) == config.LLMAuthAPIKey {
		draft.LLMCredentialStore = initCredentialStoreDraftValue(document.selectedValue(initLLMRuntimeFieldCredentialStore))
		draft.LLMCredentialRef = strings.TrimSpace(document.fieldValue(initLLMRuntimeFieldCredentialName))
	}
	return draft
}

func initLLMRuntimeSetSelectionOptions(model *initLinearEditorModel, ctx initPromptContext, selected string) {
	index := model.document.fieldIndexByID(initLLMRuntimeFieldSelection)
	if index < 0 {
		return
	}
	options := initLinearOptionsFromHuh(initLLMRuntimeLinearSelectionOptions(ctx), selected)
	for optionIndex := range options {
		option := &options[optionIndex]
		if _, configured := ctx.LLMRuntimes[option.Value]; configured {
			option.Deletable = true
		}
		if _, restorable := initLLMRuntimeRestoreSelectionName(option.Value); restorable {
			option.Restorable = true
		}
	}
	model.document[index].Options = options
}

func initLLMRuntimeSetDetailsDescription(model *initLinearEditorModel, draft initDraft, availabilityNote func(initLLMRuntimePreset) string) {
	runtime := initLLMRuntimeDraftFromSeedDraft(draft)
	detailsIndex := model.document.fieldIndexByTitle("Runtime details")
	if detailsIndex < 0 {
		return
	}
	model.document[detailsIndex].Description = initLLMRuntimeSelectionDescription(runtime, availabilityNote(runtime.Preset))
}

func initLinearOptionsFromHuh(options []huh.Option[string], selected string) []initLinearOption {
	if selected == "" {
		selected = normalizeInitStringSelectionValue("", options)
	}
	linearOptions := make([]initLinearOption, 0, len(options))
	for _, option := range options {
		linearOptions = append(linearOptions, initLinearOption{
			Label:    option.Key,
			Value:    option.Value,
			Selected: option.Value == selected,
		})
	}
	return linearOptions
}

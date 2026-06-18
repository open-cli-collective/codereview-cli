package credentialcmd

import (
	"fmt"
	"io"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
)

type initReviewerEntityEditorRunner func(initLinearEditor, io.Reader, io.Writer) (initLinearEditorModel, error)

const (
	initReviewerEntityFieldSelection      initLinearFieldID = "reviewer_entity_selection"
	initReviewerEntityFieldLabel          initLinearFieldID = "reviewer_entity_label"
	initReviewerEntityFieldSecretLocation initLinearFieldID = "reviewer_entity_secret_location"
	initReviewerEntityFieldAction         initLinearFieldID = "reviewer_entity_action"
)

const (
	initReviewerEntityActionDelete  = "delete"
	initReviewerEntityActionRestore = "restore"
)

const initReviewerEntityRestoreSelectionPrefix = "__restore_reviewer_entity__:"

func (p huhInitReviewerEntityPrompter) editReviewerEntityLinear(prompt initReviewerEntityPrompt) (initDraft, error) {
	seed := seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.DefaultProfileName, prompt.Context.ExistingProfile)
	editor := initReviewerEntityLinearEditor(prompt.Context, seed)
	model, err := p.runReviewerEntityEditor(editor)
	if err != nil {
		return initDraft{}, err
	}
	selection := model.document.selectedValue(initReviewerEntityFieldSelection)
	switch model.resultAction {
	case initDetailActionEdit:
		draft, err := initReviewerEntityDraftFromDocument(prompt.Context, seed, model.document)
		if err != nil {
			return initDraft{}, err
		}
		if _, ok := prompt.Context.ReviewerEntities[selection]; ok {
			draft.ActionTarget = selection
		}
		return draft, nil
	case initReviewerEntityActionDelete:
		return initDraft{
			Action:       initDraftActionDeleteReviewerEntity,
			ActionTarget: selection,
		}, nil
	case initReviewerEntityActionRestore:
		entityName, _ := initReviewerEntityRestoreSelectionName(selection)
		return initDraft{
			Action:       initDraftActionUndoDeleteReviewerEntity,
			ActionTarget: entityName,
		}, nil
	default:
		return initDraft{}, errInitNavigateBack
	}
}

func (p huhInitReviewerEntityPrompter) editReviewerEntityFieldsLinear(entity initReviewerEntityDraft, seed initDraft, preserveCurrentLocation bool) (initDraft, bool, error) {
	editorState, err := newReviewerEntityEditorState(entity, seed, preserveCurrentLocation)
	if err != nil {
		return initDraft{}, false, err
	}
	model, err := p.runReviewerEntityEditor(editorState.editor())
	if err != nil {
		return initDraft{}, false, err
	}
	switch model.resultAction {
	case initDetailActionEdit:
		draft, err := editorState.draftFromDocument(model.document)
		if err != nil {
			return initDraft{}, false, err
		}
		return draft, false, nil
	default:
		return initDraft{}, true, nil
	}
}

func (p huhInitReviewerEntityPrompter) runReviewerEntityEditor(editor initLinearEditor) (initLinearEditorModel, error) {
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
		return initLinearEditorModel{}, fmt.Errorf("reviewer entity editor returned %T", finalModel)
	}
	return model, nil
}

type reviewerEntityEditorState struct {
	seed                    initDraft
	kind                    initReviewerEntityKind
	explicitDisplayName     string
	fallbackLabelSeed       string
	standardReviewerRef     string
	preserveCurrentLocation bool
}

func newReviewerEntityEditorState(entity initReviewerEntityDraft, seed initDraft, preserveCurrentLocation bool) (reviewerEntityEditorState, error) {
	editDraft := seed
	kind := entity.Kind
	applyReviewerEntitySelection(&editDraft, string(kind))
	standardReviewerRef := ""
	if kind != initReviewerEntityKindUseGitIdentity {
		ref, err := standardReviewerCredentialRef(editDraft.ProfileName)
		if err != nil {
			return reviewerEntityEditorState{}, err
		}
		standardReviewerRef = ref
	}
	_, explicitDisplayName, fallbackLabelSeed := reviewerEntityEditorLabelSeed(entity)
	return reviewerEntityEditorState{
		seed:                    editDraft,
		kind:                    kind,
		explicitDisplayName:     explicitDisplayName,
		fallbackLabelSeed:       fallbackLabelSeed,
		standardReviewerRef:     standardReviewerRef,
		preserveCurrentLocation: preserveCurrentLocation,
	}, nil
}

func (s reviewerEntityEditorState) editor() initLinearEditor {
	var document initLinearDocument
	document.addSection("Reviewer entity", reviewerEntitySelectionDescription())
	document.addSection("Reviewer entity type", reviewerEntityKindDetailLabel(s.kind))
	if s.kind != initReviewerEntityKindUseGitIdentity {
		labelInput, _, _ := reviewerEntityEditorLabelSeed(initReviewerEntityDraft{
			Kind:          s.kind,
			CredentialRef: s.seed.ReviewerCredentialRef,
			DisplayName:   s.seed.ReviewerDisplayName,
		})
		if s.explicitDisplayName != "" {
			labelInput = s.explicitDisplayName
		}
		if !s.preserveCurrentLocation {
			labelInput = ""
		}
		reviewerSecretLocation := ""
		if s.preserveCurrentLocation {
			if currentRef := strings.TrimSpace(s.seed.ReviewerCredentialRef); currentRef != "" {
				reviewerSecretLocation = currentRef
			} else {
				reviewerSecretLocation = s.standardReviewerRef
			}
		}
		document.addEditableInput(
			initReviewerEntityFieldLabel,
			"Entity label",
			"Choose a human-friendly name for this reviewer entity. Leave blank to clear any existing custom label.",
			labelInput,
			validateOptionalDisplayName,
		)
		document.addEditableInput(
			initReviewerEntityFieldSecretLocation,
			"Reviewer secret location",
			"Leave blank to use the standard reviewer secret location for this profile. Replace the value only if you need a custom location.",
			reviewerSecretLocation,
			validateOptionalCredentialRef,
		)
	}
	document.addEditableSelect(initReviewerEntityFieldAction, "Reviewer detail action", "", []huh.Option[string]{
		huh.NewOption("Stage reviewer settings", initDetailActionEdit),
		huh.NewOption("Back without staging", initDetailActionBack),
	}, initDetailActionEdit)
	return initLinearEditor{
		Document: document,
		OnEnter: func(model *initLinearEditorModel) (bool, tea.Cmd) {
			if model.focused < 0 || model.focused >= len(model.document) {
				return false, nil
			}
			if model.document[model.focused].ID != initReviewerEntityFieldAction {
				return false, nil
			}
			model.document[model.focused].Error = ""
			switch model.document.selectedValue(initReviewerEntityFieldAction) {
			case initDetailActionBack:
				model.resultAction = initDetailActionBack
				return true, tea.Quit
			case initDetailActionEdit:
				if _, err := s.draftFromDocument(model.document); err != nil {
					model.document[model.focused].Error = err.Error()
					model.relayout()
					model.ensureFocusedVisible()
					return true, nil
				}
				model.resultAction = initDetailActionEdit
				return true, tea.Quit
			default:
				return true, nil
			}
		},
	}
}

func (s reviewerEntityEditorState) draftFromDocument(document initLinearDocument) (initDraft, error) {
	editDraft := s.seed
	applyReviewerEntitySelection(&editDraft, string(s.kind))
	if s.kind == initReviewerEntityKindUseGitIdentity {
		return editDraft, nil
	}
	labelInput := document.fieldValue(initReviewerEntityFieldLabel)
	if err := validateOptionalDisplayName(labelInput); err != nil {
		return initDraft{}, err
	}
	reviewerSecretLocation := document.fieldValue(initReviewerEntityFieldSecretLocation)
	if err := validateOptionalCredentialRef(reviewerSecretLocation); err != nil {
		return initDraft{}, err
	}
	finalizeReviewerEntityEditorDraft(&editDraft, s.explicitDisplayName, s.fallbackLabelSeed, labelInput, reviewerSecretLocation, s.standardReviewerRef, s.preserveCurrentLocation)
	return editDraft, nil
}

func reviewerEntityKindDetailLabel(kind initReviewerEntityKind) string {
	switch kind {
	case initReviewerEntityKindUseGitIdentity:
		return "Post using this profile's Git account"
	case initReviewerEntityKindGitHubApp:
		return "GitHub App reviewer"
	case initReviewerEntityKindPAT:
		return "Personal access token (PAT) reviewer"
	default:
		return strings.TrimSpace(string(kind))
	}
}

func initReviewerEntityLinearEditor(ctx initPromptContext, seed initDraft) initLinearEditor {
	options := initReviewerEntityLinearSelectionOptions(ctx)
	selection := initReviewerEntityDefaultSelection(ctx, seed, options)
	state, _ := reviewerEntityEditorStateForSelection(ctx, seed, selection)
	var document initLinearDocument
	document.addSection("Reviewer entity", reviewerEntitySelectionDescription())
	document.addEditableSelect(initReviewerEntityFieldSelection, "Reviewer entity", "", options, selection)
	document.addSection("Reviewer details", "")
	document.addEditableInput(
		initReviewerEntityFieldLabel,
		"Entity label",
		"Choose a human-friendly name for this reviewer entity. Leave blank to clear any existing custom label.",
		"",
		validateOptionalDisplayName,
	)
	document.addEditableInput(
		initReviewerEntityFieldSecretLocation,
		"Reviewer secret location",
		"Leave blank to use the standard reviewer secret location for this profile. Replace the value only if you need a custom location.",
		"",
		validateOptionalCredentialRef,
	)
	document.addEditableSelect(initReviewerEntityFieldAction, "Reviewer action", "", initReviewerEntityActionOptions(ctx, selection), initDetailActionEdit)
	editor := initLinearEditor{
		Document: document,
		OnFieldChange: func(model *initLinearEditorModel, index int) {
			if index < 0 || index >= len(model.document) {
				return
			}
			id := model.document[index].ID
			if id == initReviewerEntityFieldSelection {
				initReviewerEntitySyncLinearFields(model, ctx, seed, true)
				return
			}
			if id == initReviewerEntityFieldAction {
				initReviewerEntitySyncLinearFields(model, ctx, seed, false)
			}
		},
		OnEnter: func(model *initLinearEditorModel) (bool, tea.Cmd) {
			if model.focused < 0 || model.focused >= len(model.document) {
				return false, nil
			}
			if model.document[model.focused].ID != initReviewerEntityFieldAction {
				return false, nil
			}
			action := model.document.selectedValue(initReviewerEntityFieldAction)
			switch action {
			case initDetailActionBack:
				model.resultAction = initDetailActionBack
				return true, tea.Quit
			case initReviewerEntityActionDelete:
				model.resultAction = initReviewerEntityActionDelete
				return true, tea.Quit
			case initReviewerEntityActionRestore:
				model.resultAction = initReviewerEntityActionRestore
				return true, tea.Quit
			case initDetailActionEdit:
				if _, err := initReviewerEntityDraftFromDocument(ctx, seed, model.document); err != nil {
					model.document[model.focused].Error = err.Error()
					model.relayout()
					model.ensureFocusedVisible()
					return true, nil
				}
				model.resultAction = initDetailActionEdit
				return true, tea.Quit
			default:
				return true, nil
			}
		},
	}
	model := newInitLinearEditorModel(editor, 100, 28)
	_ = state
	initReviewerEntitySyncLinearFields(&model, ctx, seed, true)
	editor.Document = model.document
	return editor
}

func initReviewerEntityLinearSelectionOptions(ctx initPromptContext) []huh.Option[string] {
	options := initReviewerEntityOptions(ctx.ReviewerEntities, focusedReviewerEntityFallbackLabel(ctx.ExistingProfile))
	pendingNames := make([]string, 0, len(ctx.PendingReviewerEntityDeletes))
	for name := range ctx.PendingReviewerEntityDeletes {
		pendingNames = append(pendingNames, name)
	}
	sort.Strings(pendingNames)
	for _, name := range pendingNames {
		options = append(options, huh.NewOption(reviewerEntityDeletePendingLabel(name), initReviewerEntityRestoreSelectionPrefix+name))
	}
	return dedupeInitStringOptions(options)
}

func initReviewerEntityDefaultSelection(ctx initPromptContext, seed initDraft, options []huh.Option[string]) string {
	if selected := strings.TrimSpace(ctx.ProfileReviewerEntities[ctx.ExistingProfileName]); selected != "" {
		return normalizeInitStringSelectionValue(normalizeReviewerEntitySelectionValue(selected, ctx.ReviewerEntities), options)
	}
	current := initReviewerEntityDraftFromSeedDraft(seed)
	for name, entity := range ctx.ReviewerEntities {
		if entity.identityKey() != "" && entity.identityKey() == current.identityKey() {
			return normalizeInitStringSelectionValue(name, options)
		}
	}
	return normalizeInitStringSelectionValue(string(current.Kind), options)
}

func initReviewerEntityRestoreSelectionName(selection string) (string, bool) {
	if !strings.HasPrefix(selection, initReviewerEntityRestoreSelectionPrefix) {
		return "", false
	}
	return strings.TrimPrefix(selection, initReviewerEntityRestoreSelectionPrefix), true
}

func initReviewerEntityActionOptions(ctx initPromptContext, selection string) []huh.Option[string] {
	if _, ok := initReviewerEntityRestoreSelectionName(selection); ok {
		return []huh.Option[string]{
			huh.NewOption("Back without staging", initDetailActionBack),
		}
	}
	options := []huh.Option[string]{
		huh.NewOption("Stage reviewer settings", initDetailActionEdit),
	}
	options = append(options, huh.NewOption("Back without staging", initDetailActionBack))
	return options
}

func initReviewerEntitySyncLinearFields(model *initLinearEditorModel, ctx initPromptContext, seed initDraft, resetDetails bool) {
	selection := model.document.selectedValue(initReviewerEntityFieldSelection)
	initReviewerEntitySetSelectionOptions(model, ctx, selection)
	actionIndex := model.document.fieldIndexByID(initReviewerEntityFieldAction)
	if actionIndex >= 0 {
		selectedAction := model.document.selectedValue(initReviewerEntityFieldAction)
		model.document[actionIndex].Options = initLinearOptionsFromHuh(initReviewerEntityActionOptions(ctx, selection), selectedAction)
		if model.document.selectedValue(initReviewerEntityFieldAction) == "" {
			model.document[actionIndex].Options = initLinearOptionsFromHuh(initReviewerEntityActionOptions(ctx, selection), initDetailActionEdit)
		}
	}
	state, err := reviewerEntityEditorStateForSelection(ctx, seed, selection)
	if err != nil {
		return
	}
	detailsIndex := model.document.fieldIndexByTitle("Reviewer details")
	if detailsIndex >= 0 {
		model.document[detailsIndex].Description = reviewerEntityKindDetailLabel(state.kind)
	}
	hideDetails := state.kind == initReviewerEntityKindUseGitIdentity
	if _, restore := initReviewerEntityRestoreSelectionName(selection); restore {
		hideDetails = true
		if detailsIndex >= 0 {
			model.document[detailsIndex].Description = "This reviewer entity is staged for deletion. Restore it to make it available again."
		}
	}
	model.setFieldHidden(initReviewerEntityFieldLabel, hideDetails)
	model.setFieldHidden(initReviewerEntityFieldSecretLocation, hideDetails)
	if resetDetails && !hideDetails {
		labelInput, _, _ := reviewerEntityEditorLabelSeed(initReviewerEntityDraft{
			Kind:          state.kind,
			CredentialRef: state.seed.ReviewerCredentialRef,
			DisplayName:   state.seed.ReviewerDisplayName,
		})
		if state.explicitDisplayName != "" {
			labelInput = state.explicitDisplayName
		}
		if !state.preserveCurrentLocation {
			labelInput = ""
		}
		reviewerSecretLocation := ""
		if state.preserveCurrentLocation {
			if currentRef := strings.TrimSpace(state.seed.ReviewerCredentialRef); currentRef != "" {
				reviewerSecretLocation = currentRef
			} else {
				reviewerSecretLocation = state.standardReviewerRef
			}
		}
		model.setFieldValue(initReviewerEntityFieldLabel, labelInput)
		model.setFieldValue(initReviewerEntityFieldSecretLocation, reviewerSecretLocation)
	}
}

func initReviewerEntitySetSelectionOptions(model *initLinearEditorModel, ctx initPromptContext, selected string) {
	index := model.document.fieldIndexByID(initReviewerEntityFieldSelection)
	if index < 0 {
		return
	}
	options := initLinearOptionsFromHuh(initReviewerEntityLinearSelectionOptions(ctx), selected)
	for optionIndex := range options {
		option := &options[optionIndex]
		if _, configured := ctx.ReviewerEntities[option.Value]; configured {
			option.Deletable = true
		}
		if _, restorable := initReviewerEntityRestoreSelectionName(option.Value); restorable {
			option.Restorable = true
		}
	}
	model.document[index].Options = options
}

func reviewerEntityEditorStateForSelection(ctx initPromptContext, seed initDraft, selection string) (reviewerEntityEditorState, error) {
	if entity, ok := ctx.ReviewerEntities[selection]; ok {
		candidate := seed
		applyReviewerEntityInventorySelection(&candidate, selection, ctx.ReviewerEntities)
		return newReviewerEntityEditorState(entity, candidate, true)
	}
	if _, restore := initReviewerEntityRestoreSelectionName(selection); restore {
		return newReviewerEntityEditorState(initReviewerEntityDraft{Kind: initReviewerEntityKindUseGitIdentity}, seed, false)
	}
	candidate := seed
	applyReviewerEntityInventorySelection(&candidate, selection, ctx.ReviewerEntities)
	return newReviewerEntityEditorState(initReviewerEntityDraft{Kind: initReviewerEntityKind(selection)}, candidate, false)
}

func initReviewerEntityDraftFromDocument(ctx initPromptContext, seed initDraft, document initLinearDocument) (initDraft, error) {
	selection := document.selectedValue(initReviewerEntityFieldSelection)
	state, err := reviewerEntityEditorStateForSelection(ctx, seed, selection)
	if err != nil {
		return initDraft{}, err
	}
	return state.draftFromDocument(document)
}

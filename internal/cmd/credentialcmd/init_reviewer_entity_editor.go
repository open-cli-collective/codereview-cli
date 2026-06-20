package credentialcmd

import (
	"fmt"
	"io"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/open-cli-collective/cli-common/credstore"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
)

type initReviewerEntityEditorRunner func(initLinearEditor, io.Reader, io.Writer) (initLinearEditorModel, error)

const (
	initReviewerEntityFieldSelection               initLinearFieldID = "reviewer_entity_selection"
	initReviewerEntityFieldLabel                   initLinearFieldID = "reviewer_entity_label"
	initReviewerEntityFieldCredentialStore         initLinearFieldID = "reviewer_entity_credential_store"
	initReviewerEntityFieldSecretLocation          initLinearFieldID = "reviewer_entity_secret_location"
	initReviewerEntityFieldCredentialStatus        initLinearFieldID = "reviewer_entity_credential_status"
	initReviewerEntityFieldCredentialValues        initLinearFieldID = "reviewer_entity_credential_values"
	initReviewerEntityFieldGitToken                initLinearFieldID = "reviewer_entity_git_token"
	initReviewerEntityFieldGitHubAppID             initLinearFieldID = "reviewer_entity_github_app_id"
	initReviewerEntityFieldGitHubAppPrivateKey     initLinearFieldID = "reviewer_entity_github_app_private_key"
	initReviewerEntityFieldGitHubAppInstallationID initLinearFieldID = "reviewer_entity_github_app_installation_id"
	initReviewerEntityFieldAction                  initLinearFieldID = "reviewer_entity_action"
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

func (p huhInitReviewerEntityPrompter) editReviewerEntityFieldsLinear(ctx initPromptContext, entity initReviewerEntityDraft, seed initDraft, preserveCurrentLocation bool) (initDraft, bool, error) {
	editorState, err := newReviewerEntityEditorState(entity, seed, preserveCurrentLocation)
	if err != nil {
		return initDraft{}, false, err
	}
	model, err := p.runReviewerEntityEditor(editorState.editor(ctx))
	if err != nil {
		return initDraft{}, false, err
	}
	switch model.resultAction {
	case initDetailActionEdit:
		draft, err := editorState.draftFromDocument(ctx, model.document)
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

func (s reviewerEntityEditorState) editor(ctx initPromptContext) initLinearEditor {
	var document initLinearDocument
	document.addSection("Reviewer entity", reviewerEntitySelectionDescription())
	document.addSection("Reviewer entity type", reviewerEntityKindDetailDescription(s.kind))
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
		reviewerSecretLocation := s.standardReviewerRef
		if s.preserveCurrentLocation {
			if currentRef := strings.TrimSpace(s.seed.ReviewerCredentialRef); currentRef != "" {
				reviewerSecretLocation = currentRef
			}
		}
		document.addEditableInput(
			initReviewerEntityFieldLabel,
			"Entity label",
			"Choose a human-friendly name for this reviewer entity. Leave blank to clear any existing custom label.",
			labelInput,
			validateOptionalDisplayName,
		)
		document.addEditableSelect(
			initReviewerEntityFieldCredentialStore,
			"Reviewer credential store",
			"",
			initCredentialStoreOptions(ctx.ExistingConfig),
			normalizeInitStringSelectionValue(initCredentialStoreDraftValue(s.seed.ReviewerCredentialStore), initCredentialStoreOptions(ctx.ExistingConfig)),
		)
		document.addEditableInput(
			initReviewerEntityFieldSecretLocation,
			"Reviewer secret location",
			reviewerEntitySecretLocationDescription(s.kind),
			reviewerSecretLocation,
			validateOptionalCredentialRef,
		)
		if locationIndex := document.fieldIndexByID(initReviewerEntityFieldSecretLocation); locationIndex >= 0 {
			document[locationIndex].AutoManaged = !s.preserveCurrentLocation
		}
		status := synthesizeReviewerCredentialStatus(ctx, config.CredentialRef{
			Purpose: "reviewer_credentials",
			Store:   initCredentialStoreDraftValue(s.seed.ReviewerCredentialStore),
			Ref:     firstNonEmpty(reviewerSecretLocation, s.standardReviewerRef),
			Mode:    string(reviewerCredentialAuthModeForKind(s.kind)),
		})
		document.addSectionField(
			initReviewerEntityFieldCredentialStatus,
			"Reviewer credential status",
			initReviewerCredentialStatusDescription(status),
		)
		addReviewerEntityCredentialValueFields(&document, s.kind, false)
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
				if _, err := s.draftFromDocument(ctx, model.document); err != nil {
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
		OnFieldChange: func(model *initLinearEditorModel, index int) {
			if index < 0 || index >= len(model.document) || s.kind == initReviewerEntityKindUseGitIdentity {
				return
			}
			id := model.document[index].ID
			if id == initReviewerEntityFieldSecretLocation {
				model.document[index].AutoManaged = false
			}
			if id == initReviewerEntityFieldLabel {
				initReviewerEntityMaybeUpdateAutoSecretLocation(model, s)
			}
			if id == initReviewerEntityFieldLabel ||
				id == initReviewerEntityFieldCredentialStore ||
				id == initReviewerEntityFieldSecretLocation ||
				initReviewerEntityCredentialFieldKey(id) != "" {
				initReviewerEntityRefreshCredentialStatus(model, ctx, s.seed, s)
			}
		},
	}
}

func (s reviewerEntityEditorState) draftFromDocument(ctx initPromptContext, document initLinearDocument) (initDraft, error) {
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
	editDraft.ReviewerCredentialStore = initCredentialStoreDraftValue(document.selectedValue(initReviewerEntityFieldCredentialStore))
	if err := validateReviewerEntityInlineCredentials(ctx, s.seed, s, document); err != nil {
		return initDraft{}, err
	}
	finalizeReviewerEntityEditorDraft(&editDraft, s.explicitDisplayName, s.fallbackLabelSeed, labelInput, reviewerSecretLocation, s.standardReviewerRef, s.preserveCurrentLocation)
	applyReviewerEntityCredentialDraftFromDocument(&editDraft, ctx, s.seed, s, document)
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
	document.addEditableSelect(
		initReviewerEntityFieldCredentialStore,
		"Reviewer credential store",
		"",
		initCredentialStoreOptions(ctx.ExistingConfig),
		normalizeInitStringSelectionValue(initCredentialStoreDraftValue(state.seed.ReviewerCredentialStore), initCredentialStoreOptions(ctx.ExistingConfig)),
	)
	document.addEditableInput(
		initReviewerEntityFieldSecretLocation,
		"Reviewer secret location",
		reviewerEntitySecretLocationDescription(state.kind),
		"",
		validateOptionalCredentialRef,
	)
	document.addSectionField(initReviewerEntityFieldCredentialStatus, "Reviewer credential status", "", initLinearFieldOptions{Hidden: true})
	addReviewerEntityCredentialValueFields(&document, state.kind, true)
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
			if id == initReviewerEntityFieldSecretLocation {
				model.document[index].AutoManaged = false
			}
			if id == initReviewerEntityFieldAction ||
				id == initReviewerEntityFieldLabel ||
				id == initReviewerEntityFieldCredentialStore ||
				id == initReviewerEntityFieldSecretLocation ||
				initReviewerEntityCredentialFieldKey(id) != "" {
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

func initReviewerEntityActionOptions(_ initPromptContext, selection string) []huh.Option[string] {
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

func addReviewerEntityCredentialValueFields(document *initLinearDocument, kind initReviewerEntityKind, hidden bool) {
	document.addSectionField(
		initReviewerEntityFieldCredentialValues,
		"Reviewer credential values",
		"Enter required reviewer secrets here. Values are hidden, staged in memory, and written only when you Commit staged changes and exit.",
		initLinearFieldOptions{Hidden: hidden},
	)
	document.addEditableSecretInput(
		initReviewerEntityFieldGitToken,
		"GitHub PAT",
		"Required for PAT reviewers. Leave blank only when the status above already shows git_token as existing or staged.",
		"",
		nil,
		initLinearFieldOptions{Hidden: hidden || kind != initReviewerEntityKindPAT},
	)
	document.addEditableSecretInput(
		initReviewerEntityFieldGitHubAppID,
		"GitHub App ID",
		"Required for GitHub App reviewers. This is stored as github_app_id at the destination above.",
		"",
		nil,
		initLinearFieldOptions{Hidden: hidden || kind != initReviewerEntityKindGitHubApp},
	)
	document.addEditableSecretTextarea(
		initReviewerEntityFieldGitHubAppPrivateKey,
		"GitHub App private key",
		"Required for GitHub App reviewers. Paste the PEM private key; ctrl+j or alt+enter inserts a newline.",
		"",
	)
	if index := document.fieldIndexByID(initReviewerEntityFieldGitHubAppPrivateKey); index >= 0 {
		(*document)[index].Hidden = hidden || kind != initReviewerEntityKindGitHubApp
	}
	document.addEditableSecretInput(
		initReviewerEntityFieldGitHubAppInstallationID,
		"GitHub App installation ID",
		"Optional. Leave blank unless this reviewer must pin a specific installation.",
		"",
		nil,
		initLinearFieldOptions{Hidden: hidden || kind != initReviewerEntityKindGitHubApp},
	)
}

func initReviewerEntitySetCredentialFieldsHidden(model *initLinearEditorModel, kind initReviewerEntityKind, hideDetails bool) {
	hidePAT := hideDetails || kind != initReviewerEntityKindPAT
	hideGitHubApp := hideDetails || kind != initReviewerEntityKindGitHubApp
	model.setFieldHidden(initReviewerEntityFieldCredentialValues, hideDetails)
	model.setFieldHidden(initReviewerEntityFieldGitToken, hidePAT)
	model.setFieldHidden(initReviewerEntityFieldGitHubAppID, hideGitHubApp)
	model.setFieldHidden(initReviewerEntityFieldGitHubAppPrivateKey, hideGitHubApp)
	model.setFieldHidden(initReviewerEntityFieldGitHubAppInstallationID, hideGitHubApp)
}

func initReviewerEntityClearCredentialFieldValues(model *initLinearEditorModel) {
	for _, id := range []initLinearFieldID{
		initReviewerEntityFieldGitToken,
		initReviewerEntityFieldGitHubAppID,
		initReviewerEntityFieldGitHubAppPrivateKey,
		initReviewerEntityFieldGitHubAppInstallationID,
	} {
		model.setFieldValue(id, "")
	}
}

func initReviewerEntityMaybeUpdateAutoSecretLocation(model *initLinearEditorModel, state reviewerEntityEditorState) {
	locationIndex := model.document.fieldIndexByID(initReviewerEntityFieldSecretLocation)
	if locationIndex < 0 || !model.document[locationIndex].AutoManaged {
		return
	}
	model.setFieldValue(initReviewerEntityFieldSecretLocation, initReviewerEntityDefaultSecretLocation(state, model.document.fieldValue(initReviewerEntityFieldLabel)))
	if locationIndex = model.document.fieldIndexByID(initReviewerEntityFieldSecretLocation); locationIndex >= 0 {
		model.document[locationIndex].AutoManaged = true
	}
}

func initReviewerEntityDefaultSecretLocation(state reviewerEntityEditorState, labelInput string) string {
	if state.preserveCurrentLocation {
		if currentRef := strings.TrimSpace(state.seed.ReviewerCredentialRef); currentRef != "" {
			return currentRef
		}
		return state.standardReviewerRef
	}
	if ref, ok := reviewerCredentialRefFromLabel(labelInput); ok {
		return ref
	}
	return state.standardReviewerRef
}

func reviewerCredentialRefFromLabel(label string) (string, bool) {
	segment := credstore.EscapeRefSegment(strings.TrimSpace(label))
	if segment == "" {
		return "", false
	}
	ref, err := credentials.FormatRef(segment + "-reviewer")
	if err != nil {
		return "", false
	}
	return ref, true
}

func initReviewerEntityCredentialFieldKey(id initLinearFieldID) string {
	// This mapper intentionally accepts every linear-editor field ID but only reviewer
	// credential value fields produce credential-store keys.
	//nolint:exhaustive
	switch id {
	case initReviewerEntityFieldGitToken:
		return credentials.GitTokenKey
	case initReviewerEntityFieldGitHubAppID:
		return credentials.GitHubAppIDKey
	case initReviewerEntityFieldGitHubAppPrivateKey:
		return credentials.GitHubAppPrivateKeyKey
	case initReviewerEntityFieldGitHubAppInstallationID:
		return credentials.GitHubAppInstallationIDKey
	default:
		return ""
	}
}

func initReviewerEntityCredentialFieldID(key string) initLinearFieldID {
	switch key {
	case credentials.GitTokenKey:
		return initReviewerEntityFieldGitToken
	case credentials.GitHubAppIDKey:
		return initReviewerEntityFieldGitHubAppID
	case credentials.GitHubAppPrivateKeyKey:
		return initReviewerEntityFieldGitHubAppPrivateKey
	case credentials.GitHubAppInstallationIDKey:
		return initReviewerEntityFieldGitHubAppInstallationID
	default:
		return ""
	}
}

func initReviewerCredentialStatusWithDocumentWrites(status initReviewerCredentialStatus, document initLinearDocument) initReviewerCredentialStatus {
	status.Keys = append([]initReviewerCredentialKeyStatus(nil), status.Keys...)
	for index, key := range status.Keys {
		fieldID := initReviewerEntityCredentialFieldID(key.Key)
		if fieldID == "" || document.fieldHidden(fieldID) {
			continue
		}
		if strings.TrimSpace(credentials.TrimSecretIngress(document.fieldValue(fieldID))) == "" {
			continue
		}
		status.Keys[index].State = initReviewerCredentialKeyStaged
	}
	return status
}

func validateReviewerEntityInlineCredentials(ctx initPromptContext, seed initDraft, state reviewerEntityEditorState, document initLinearDocument) error {
	if state.kind == initReviewerEntityKindUseGitIdentity {
		return nil
	}
	ref := strings.TrimSpace(document.fieldValue(initReviewerEntityFieldSecretLocation))
	if ref == "" {
		return fmt.Errorf("reviewer secret location is required")
	}
	if strings.TrimSpace(document.selectedValue(initReviewerEntityFieldCredentialStore)) == "" {
		return fmt.Errorf("reviewer credential store is required")
	}
	status, ok := reviewerCredentialStatusForDocument(ctx, seed, state, document)
	if !ok {
		return fmt.Errorf("reviewer credential status unavailable")
	}
	if reviewerEntityPreservesExistingCredentialRefWithoutWrites(state, document) {
		return nil
	}
	missing := missingReviewerEntityInlineCredentialKeys(state, status, document)
	if len(missing) > 0 {
		return fmt.Errorf("set reviewer credentials before staging: %s", strings.Join(missing, ", "))
	}
	return nil
}

func reviewerEntityPreservesExistingCredentialRefWithoutWrites(state reviewerEntityEditorState, document initLinearDocument) bool {
	if !state.preserveCurrentLocation {
		return false
	}
	if strings.TrimSpace(document.fieldValue(initReviewerEntityFieldSecretLocation)) != strings.TrimSpace(state.seed.ReviewerCredentialRef) {
		return false
	}
	if initCredentialStoreDraftValue(document.selectedValue(initReviewerEntityFieldCredentialStore)) != initCredentialStoreDraftValue(state.seed.ReviewerCredentialStore) {
		return false
	}
	writes, _ := reviewerEntityCredentialWritesFromDocument(synthesizeReviewerCredentialStatus(initPromptContext{}, config.CredentialRef{
		Purpose: "reviewer_credentials",
		Store:   initCredentialStoreDraftValue(state.seed.ReviewerCredentialStore),
		Ref:     strings.TrimSpace(state.seed.ReviewerCredentialRef),
		Mode:    string(reviewerCredentialAuthModeForKind(state.kind)),
	}), document)
	return len(writes) == 0
}

func reviewerCredentialStatusForDocument(ctx initPromptContext, seed initDraft, state reviewerEntityEditorState, document initLinearDocument) (initReviewerCredentialStatus, bool) {
	status, ok := baseReviewerCredentialStatusForDocument(ctx, seed, state, document)
	if !ok {
		return initReviewerCredentialStatus{}, false
	}
	return initReviewerCredentialStatusWithDocumentWrites(status, document), true
}

func baseReviewerCredentialStatusForDocument(ctx initPromptContext, seed initDraft, state reviewerEntityEditorState, document initLinearDocument) (initReviewerCredentialStatus, bool) {
	status, ok := initReviewerCredentialStatusForSelectionRef(ctx, seed, string(state.kind), document.selectedValue(initReviewerEntityFieldCredentialStore), document.fieldValue(initReviewerEntityFieldSecretLocation))
	if !ok {
		return initReviewerCredentialStatus{}, false
	}
	if initReviewerEntityUsesAutoDefaultSecretLocation(state, document) && !initReviewerCredentialStatusListContains(ctx.ReviewerCredentialStatuses, status.Ref) {
		if initReviewerCredentialStatusListContainsUnavailable(ctx.ReviewerCredentialStatuses) {
			status = synthesizeUnavailableReviewerCredentialStatus(ctx, status.Ref)
		} else {
			status = synthesizeReviewerCredentialStatus(ctx, status.Ref)
		}
	}
	return status, true
}

func initReviewerEntityUsesAutoDefaultSecretLocation(state reviewerEntityEditorState, document initLinearDocument) bool {
	if state.preserveCurrentLocation {
		return false
	}
	locationIndex := document.fieldIndexByID(initReviewerEntityFieldSecretLocation)
	if locationIndex < 0 || !document[locationIndex].AutoManaged {
		return false
	}
	return strings.TrimSpace(document.fieldValue(initReviewerEntityFieldSecretLocation)) == strings.TrimSpace(initReviewerEntityDefaultSecretLocation(state, document.fieldValue(initReviewerEntityFieldLabel)))
}

func initReviewerCredentialStatusListContains(statuses []initReviewerCredentialStatus, ref config.CredentialRef) bool {
	for _, status := range statuses {
		if status.Ref.Purpose == ref.Purpose &&
			initCredentialStoreDraftValue(status.Ref.Store) == initCredentialStoreDraftValue(ref.Store) &&
			status.Ref.Ref == ref.Ref &&
			status.Ref.Mode == ref.Mode &&
			status.Ref.Provider == ref.Provider {
			return true
		}
	}
	return false
}

func initReviewerCredentialStatusListContainsUnavailable(statuses []initReviewerCredentialStatus) bool {
	for _, status := range statuses {
		if strings.TrimSpace(status.Unavailable) != "" {
			return true
		}
		for _, key := range status.Keys {
			if key.State == initReviewerCredentialKeyUnavailable {
				return true
			}
		}
	}
	return false
}

func missingReviewerEntityInlineCredentialKeys(state reviewerEntityEditorState, status initReviewerCredentialStatus, document initLinearDocument) []string {
	var missing []string
	keepsCurrentRef := reviewerEntityKeepsCurrentCredentialRef(state, document)
	for _, key := range status.Keys {
		if !key.Required {
			continue
		}
		switch key.State {
		case initReviewerCredentialKeyExisting, initReviewerCredentialKeyStaged:
			continue
		case initReviewerCredentialKeyUnavailable:
			if keepsCurrentRef {
				continue
			}
		case initReviewerCredentialKeyMissing, initReviewerCredentialKeyDeferred, initReviewerCredentialKeySkippedOptional, initReviewerCredentialKeyOptional:
		}
		missing = append(missing, key.Key)
	}
	return missing
}

func reviewerEntityKeepsCurrentCredentialRef(state reviewerEntityEditorState, document initLinearDocument) bool {
	return state.preserveCurrentLocation &&
		initCredentialStoreDraftValue(document.selectedValue(initReviewerEntityFieldCredentialStore)) == initCredentialStoreDraftValue(state.seed.ReviewerCredentialStore) &&
		strings.TrimSpace(document.fieldValue(initReviewerEntityFieldSecretLocation)) == strings.TrimSpace(state.seed.ReviewerCredentialRef)
}

func applyReviewerEntityCredentialDraftFromDocument(editDraft *initDraft, ctx initPromptContext, seed initDraft, state reviewerEntityEditorState, document initLinearDocument) {
	originalStatus, ok := baseReviewerCredentialStatusForDocument(ctx, seed, state, document)
	if !ok {
		return
	}
	status := initReviewerCredentialStatusWithDocumentWrites(originalStatus, document)
	ref := strings.TrimSpace(status.Ref.Ref)
	if ref == "" {
		ref = strings.TrimSpace(document.fieldValue(initReviewerEntityFieldSecretLocation))
	}
	if ref == "" {
		return
	}
	writes, overwrite := reviewerEntityCredentialWritesFromDocument(originalStatus, document)
	editDraft.ReviewerCredentialWriteRef = ref
	editDraft.ReviewerCredentialStore = initCredentialStoreDraftValue(status.Ref.Store)
	editDraft.ReviewerCredentialWriteStore = status.SecretsProfile
	editDraft.ReviewerCredentialWrites = writes
	editDraft.ReviewerCredentialOverwrite = overwrite
	editDraft.ReviewerCredentialSatisfied = reviewerEntityPreservesExistingCredentialRefWithoutWrites(state, document) || len(missingReviewerEntityInlineCredentialKeys(state, status, document)) == 0
}

func reviewerEntityCredentialWritesFromDocument(status initReviewerCredentialStatus, document initLinearDocument) (map[string]string, bool) {
	writes := map[string]string{}
	keyStates := make(map[string]initReviewerCredentialKeyState, len(status.Keys))
	for _, key := range status.Keys {
		keyStates[key.Key] = key.State
	}
	overwrite := false
	for _, key := range status.Keys {
		fieldID := initReviewerEntityCredentialFieldID(key.Key)
		if fieldID == "" || document.fieldHidden(fieldID) {
			continue
		}
		value := credentials.TrimSecretIngress(document.fieldValue(fieldID))
		if strings.TrimSpace(value) == "" {
			continue
		}
		writes[key.Key] = value
		switch keyStates[key.Key] {
		case initReviewerCredentialKeyExisting:
			overwrite = true
		case initReviewerCredentialKeyMissing, initReviewerCredentialKeyStaged, initReviewerCredentialKeyDeferred, initReviewerCredentialKeySkippedOptional, initReviewerCredentialKeyOptional, initReviewerCredentialKeyUnavailable:
		}
	}
	return writes, overwrite
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
		model.document[detailsIndex].Description = reviewerEntityKindDetailDescription(state.kind)
	}
	hideDetails := state.kind == initReviewerEntityKindUseGitIdentity
	if _, restore := initReviewerEntityRestoreSelectionName(selection); restore {
		hideDetails = true
		if detailsIndex >= 0 {
			model.document[detailsIndex].Description = "This reviewer entity is staged for deletion. Restore it to make it available again."
		}
	}
	model.setFieldHidden(initReviewerEntityFieldLabel, hideDetails)
	model.setFieldHidden(initReviewerEntityFieldCredentialStore, hideDetails)
	model.setFieldHidden(initReviewerEntityFieldSecretLocation, hideDetails)
	model.setFieldHidden(initReviewerEntityFieldCredentialStatus, hideDetails)
	initReviewerEntitySetCredentialFieldsHidden(model, state.kind, hideDetails)
	if !hideDetails {
		model.setFieldDescription(initReviewerEntityFieldSecretLocation, reviewerEntitySecretLocationDescription(state.kind))
	}
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
		reviewerSecretLocation := state.standardReviewerRef
		if state.preserveCurrentLocation {
			if currentRef := strings.TrimSpace(state.seed.ReviewerCredentialRef); currentRef != "" {
				reviewerSecretLocation = currentRef
			}
		} else {
			reviewerSecretLocation = initReviewerEntityDefaultSecretLocation(state, labelInput)
		}
		model.setFieldValue(initReviewerEntityFieldLabel, labelInput)
		model.selectFieldValue(initReviewerEntityFieldCredentialStore, initCredentialStoreDraftValue(state.seed.ReviewerCredentialStore))
		model.setFieldValue(initReviewerEntityFieldSecretLocation, reviewerSecretLocation)
		if locationIndex := model.document.fieldIndexByID(initReviewerEntityFieldSecretLocation); locationIndex >= 0 {
			model.document[locationIndex].AutoManaged = !state.preserveCurrentLocation
		}
		initReviewerEntityClearCredentialFieldValues(model)
	}
	if !hideDetails && !resetDetails {
		initReviewerEntityMaybeUpdateAutoSecretLocation(model, state)
	}
	if !hideDetails {
		initReviewerEntityRefreshCredentialStatus(model, ctx, seed, state)
	}
}

func initReviewerEntityRefreshCredentialStatus(model *initLinearEditorModel, ctx initPromptContext, seed initDraft, state reviewerEntityEditorState) {
	if status, ok := reviewerCredentialStatusForDocument(ctx, seed, state, model.document); ok {
		model.setFieldDescription(initReviewerEntityFieldCredentialStatus, initReviewerCredentialStatusDescription(status))
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

func reviewerEntityKindDetailDescription(kind initReviewerEntityKind) string {
	label := reviewerEntityKindDetailLabel(kind)
	if kind != initReviewerEntityKindGitHubApp {
		return label
	}
	return label + ". Required credential keys: " + credentials.GitHubAppIDKey + ", " + credentials.GitHubAppPrivateKeyKey + ". Optional credential key: " + credentials.GitHubAppInstallationIDKey + "."
}

func reviewerEntitySecretLocationDescription(kind initReviewerEntityKind) string {
	base := "Credential name for this reviewer. This is where cr stores secret values in the selected credential store; it is not a PAT, GitHub App ID, private key, or installation ID. Change it only if you need a custom credential-store location."
	if kind == initReviewerEntityKindPAT {
		return base + " Store required secret " + credentials.GitTokenKey + " at this name."
	}
	if kind == initReviewerEntityKindGitHubApp {
		return base + " Store required secrets " + credentials.GitHubAppIDKey + " and " + credentials.GitHubAppPrivateKeyKey + " at this name; " + credentials.GitHubAppInstallationIDKey + " is optional."
	}
	return base
}

func initReviewerEntityDraftFromDocument(ctx initPromptContext, seed initDraft, document initLinearDocument) (initDraft, error) {
	selection := document.selectedValue(initReviewerEntityFieldSelection)
	state, err := reviewerEntityEditorStateForSelection(ctx, seed, selection)
	if err != nil {
		return initDraft{}, err
	}
	editDraft := state.seed
	applyReviewerEntitySelection(&editDraft, string(state.kind))
	if state.kind == initReviewerEntityKindUseGitIdentity {
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
	editDraft.ReviewerCredentialStore = initCredentialStoreDraftValue(document.selectedValue(initReviewerEntityFieldCredentialStore))
	if err := validateReviewerEntityInlineCredentials(ctx, seed, state, document); err != nil {
		return initDraft{}, err
	}
	finalizeReviewerEntityEditorDraft(&editDraft, state.explicitDisplayName, state.fallbackLabelSeed, labelInput, reviewerSecretLocation, state.standardReviewerRef, state.preserveCurrentLocation)
	applyReviewerEntityCredentialDraftFromDocument(&editDraft, ctx, seed, state, document)
	return editDraft, nil
}

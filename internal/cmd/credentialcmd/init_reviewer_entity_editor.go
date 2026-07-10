package credentialcmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/open-cli-collective/cli-common/credstore"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
)

const (
	initReviewerEntityFieldSelection           initLinearFieldID = "reviewer_entity_selection"
	initReviewerEntityFieldLabel               initLinearFieldID = "reviewer_entity_label"
	initReviewerEntityFieldCredentialStore     initLinearFieldID = "reviewer_entity_credential_store"
	initReviewerEntityFieldSecretLocation      initLinearFieldID = "reviewer_entity_secret_location"
	initReviewerEntityFieldCredentialStatus    initLinearFieldID = "reviewer_entity_credential_status"
	initReviewerEntityFieldGitHubAppDetails    initLinearFieldID = "reviewer_entity_github_app_details"
	initReviewerEntityFieldCredentialValues    initLinearFieldID = "reviewer_entity_credential_values"
	initReviewerEntityFieldGitToken            initLinearFieldID = "reviewer_entity_git_token"
	initReviewerEntityFieldGitHubAppID         initLinearFieldID = "reviewer_entity_github_app_id"
	initReviewerEntityFieldGitHubAppPrivateKey initLinearFieldID = "reviewer_entity_github_app_private_key"
	initReviewerEntityFieldAction              initLinearFieldID = "reviewer_entity_action"
)

func (p huhInitReviewerEntityPrompter) editReviewerEntityLinear(prompt initReviewerEntityPrompt) (initDraft, error) {
	seed := seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.ExistingProfile)
	editor := initReviewerEntityLinearEditor(prompt.Context, seed)
	model, err := runInitEditor(editor, p.stdin, p.stderr, p.editorRunner, "reviewer entity")
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
	case initLinearResultActionDelete:
		return initDraft{
			Action:       initDraftActionDeleteReviewerEntity,
			ActionTarget: selection,
		}, nil
	case initLinearResultActionRestore:
		entityName, _ := initLinearRestoreSelectionName("reviewer_entity", selection)
		return initDraft{
			Action:       initDraftActionUndoDeleteReviewerEntity,
			ActionTarget: entityName,
		}, nil
	default:
		return initDraft{}, errInitNavigateBack
	}
}

func (p huhInitReviewerEntityPrompter) editReviewerEntityFieldsLinear(ctx initPromptContext, entity initReviewerEntityDraft, seed initDraft, preserveCurrentLocation bool) (initDraft, bool, error) {
	editorState, err := newReviewerEntityEditorState(entity, seed, preserveCurrentLocation, ctx.StandaloneReviewerEntityMode)
	if err != nil {
		return initDraft{}, false, err
	}
	model, err := runInitEditor(editorState.editor(ctx), p.stdin, p.stderr, p.editorRunner, "reviewer entity")
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

type reviewerEntityEditorState struct {
	seed                    initDraft
	kind                    initReviewerEntityKind
	explicitDisplayName     string
	fallbackLabelSeed       string
	standardReviewerRef     string
	preserveCurrentLocation bool
}

func newReviewerEntityEditorState(entity initReviewerEntityDraft, seed initDraft, preserveCurrentLocation bool, standaloneReviewerEntityMode bool) (reviewerEntityEditorState, error) {
	editDraft := seed
	kind := entity.Kind
	applyReviewerEntitySelection(&editDraft, string(kind))
	if kind == initReviewerEntityKindGitHubApp {
		editDraft.ReviewerGitHubAppID = strings.TrimSpace(entity.AppID)
	} else {
		editDraft.ReviewerGitHubAppID = ""
	}
	standardReviewerRef := ""
	if kind != initReviewerEntityKindUseGitIdentity {
		if preserveCurrentLocation && standaloneReviewerEntityMode && strings.TrimSpace(editDraft.ReviewerCredentialRef) != "" {
			standardReviewerRef = strings.TrimSpace(editDraft.ReviewerCredentialRef)
		} else if !standaloneReviewerEntityMode {
			ref, err := standardReviewerCredentialRef(editDraft.ProfileName)
			if err != nil {
				return reviewerEntityEditorState{}, err
			}
			standardReviewerRef = ref
		} else {
			ref, err := standardReviewerCredentialRefForEntity(entity)
			if err != nil {
				return reviewerEntityEditorState{}, err
			}
			standardReviewerRef = ref
		}
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

func (s reviewerEntityEditorState) fieldSeeds() (string, string) {
	label, _, _ := reviewerEntityEditorLabelSeed(initReviewerEntityDraft{
		Kind:          s.kind,
		CredentialRef: s.seed.ReviewerCredentialRef,
		DisplayName:   s.seed.ReviewerDisplayName,
	})
	if s.explicitDisplayName != "" {
		label = s.explicitDisplayName
	}
	if !s.preserveCurrentLocation {
		label = ""
	}
	return label, initReviewerEntityDefaultSecretLocation(s, label)
}

func (s reviewerEntityEditorState) editor(ctx initPromptContext) initLinearEditor {
	var document initLinearDocument
	document.addSection("Reviewer entity", reviewerEntitySelectionDescription())
	document.addSection("Reviewer entity type", reviewerEntityKindDetailDescription(s.kind))
	if s.kind != initReviewerEntityKindUseGitIdentity {
		labelInput, reviewerSecretLocation := s.fieldSeeds()
		document.addEditableInput(
			initReviewerEntityFieldLabel,
			"Entity label",
			"Choose a human-friendly name for this reviewer entity. Leave blank to clear any existing custom label.",
			labelInput,
			validateOptionalDisplayName,
		)
		addReviewerEntityGitHubAppConfigFields(&document, s.kind, s.seed.ReviewerGitHubAppID, false)
		document.addEditableSelect(
			initReviewerEntityFieldCredentialStore,
			"Reviewer credential store",
			"",
			initCredentialStoreOptions(ctx.ExistingConfig),
			normalizeInitStringSelectionValue(initCredentialStoreDraftValue(s.seed.ReviewerCredentialStore), initCredentialStoreOptions(ctx.ExistingConfig)),
		)
		document.addEditableInput(
			initReviewerEntityFieldSecretLocation,
			reviewerEntitySecretLocationTitle(s.kind),
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
			reviewerEntityCredentialStatusTitle(s.kind),
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
		OnEnter: initLinearActionEnterHandler(initReviewerEntityFieldAction, func(model *initLinearEditorModel, action string) (string, error) {
			model.document[model.focused].Error = ""
			switch action {
			case initDetailActionBack:
				return initDetailActionBack, nil
			case initDetailActionEdit:
				if _, err := s.draftFromDocument(ctx, model.document); err != nil {
					return "", err
				}
				return initDetailActionEdit, nil
			default:
				return "", nil
			}
		}),
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
	appID, err := reviewerEntityGitHubAppIDFromDocument(s.kind, document)
	if err != nil {
		return initDraft{}, err
	}
	editDraft.ReviewerGitHubAppID = appID
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
	document.addEditableSelect(initReviewerEntityFieldSelection, initReviewerEntitySelectionFieldTitle(ctx), initReviewerEntitySelectionFieldDescription(ctx), options, selection)
	document.addSection("Reviewer details", "")
	document.addEditableInput(
		initReviewerEntityFieldLabel,
		"Entity label",
		"Choose a human-friendly name for this reviewer entity. Leave blank to clear any existing custom label.",
		"",
		validateOptionalDisplayName,
	)
	addReviewerEntityGitHubAppConfigFields(&document, state.kind, state.seed.ReviewerGitHubAppID, true)
	document.addEditableSelect(
		initReviewerEntityFieldCredentialStore,
		"Reviewer credential store",
		"",
		initCredentialStoreOptions(ctx.ExistingConfig),
		normalizeInitStringSelectionValue(initCredentialStoreDraftValue(state.seed.ReviewerCredentialStore), initCredentialStoreOptions(ctx.ExistingConfig)),
	)
	document.addEditableInput(
		initReviewerEntityFieldSecretLocation,
		reviewerEntitySecretLocationTitle(state.kind),
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
		OnEnter: initLinearActionEnterHandler(initReviewerEntityFieldAction, func(model *initLinearEditorModel, action string) (string, error) {
			switch action {
			case initDetailActionBack:
				return initDetailActionBack, nil
			case initLinearResultActionDelete:
				return initLinearResultActionDelete, nil
			case initLinearResultActionRestore:
				return initLinearResultActionRestore, nil
			case initDetailActionEdit:
				if _, err := initReviewerEntityDraftFromDocument(ctx, seed, model.document); err != nil {
					return "", err
				}
				return initDetailActionEdit, nil
			default:
				return "", nil
			}
		}),
	}
	model := newInitLinearEditorModel(editor, 100, 28)
	_ = state
	initReviewerEntitySyncLinearFields(&model, ctx, seed, true)
	editor.Document = model.document
	return editor
}

func initReviewerEntityLinearSelectionOptions(ctx initPromptContext) []huh.Option[string] {
	options := initReviewerEntityOptionsForContext(ctx)
	pendingNames := make([]string, 0, len(ctx.PendingReviewerEntityDeletes))
	for name := range ctx.PendingReviewerEntityDeletes {
		pendingNames = append(pendingNames, name)
	}
	sort.Strings(pendingNames)
	for _, name := range pendingNames {
		options = append(options, huh.NewOption(reviewerEntityDeletePendingLabel(name), initLinearRestoreSelection("reviewer_entity", name)))
	}
	return dedupeInitStringOptions(options)
}

func initReviewerEntityOptionsForContext(ctx initPromptContext) []huh.Option[string] {
	if !ctx.StandaloneReviewerEntityMode {
		return initReviewerEntityOptions(ctx.ReviewerEntities, focusedReviewerEntityFallbackLabel(ctx.ExistingProfile))
	}
	names := configuredInitReviewerEntityNames(ctx.ReviewerEntities)
	sort.Strings(names)
	options := make([]huh.Option[string], 0, len(names)+2)
	for _, name := range names {
		entity := ctx.ReviewerEntities[name]
		options = append(options, huh.NewOption(initReviewerEntityLabel(entity), name))
	}
	options = append(options,
		huh.NewOption(reviewerEntityTemplatePATLabel(), string(initReviewerEntityKindPAT)),
		huh.NewOption(reviewerEntityTemplateGitHubAppLabel(), string(initReviewerEntityKindGitHubApp)),
	)
	return dedupeInitStringOptions(options)
}

func initReviewerEntitySelectionFieldTitle(ctx initPromptContext) string {
	if ctx.StandaloneReviewerEntityMode {
		return "Actions"
	}
	return "Reviewer entity"
}

func initReviewerEntitySelectionFieldDescription(ctx initPromptContext) string {
	if ctx.StandaloneReviewerEntityMode {
		return "Configure a new reviewer entity or edit a configured reviewer entity."
	}
	return ""
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

func initReviewerEntityActionOptions(_ initPromptContext, selection string) []huh.Option[string] {
	if _, ok := initLinearRestoreSelectionName("reviewer_entity", selection); ok {
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

func addReviewerEntityGitHubAppConfigFields(document *initLinearDocument, kind initReviewerEntityKind, appID string, hidden bool) {
	hideGitHubApp := hidden || kind != initReviewerEntityKindGitHubApp
	document.addSectionField(
		initReviewerEntityFieldGitHubAppDetails,
		"GitHub App details",
		"",
		initLinearFieldOptions{Hidden: hideGitHubApp},
	)
	document.addEditableInput(
		initReviewerEntityFieldGitHubAppID,
		"GitHub App ID",
		"Numeric GitHub App ID. This is not a secret and is saved in config.yml.",
		strings.TrimSpace(appID),
		validateOptionalDecimalID("GitHub App ID"),
		initLinearFieldOptions{Hidden: hideGitHubApp},
	)
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
		"Enter a value for this secret.",
		"",
		nil,
		initLinearFieldOptions{Hidden: hidden || kind != initReviewerEntityKindPAT},
	)
	document.addEditableSecretTextarea(
		initReviewerEntityFieldGitHubAppPrivateKey,
		"GitHub App private key",
		"Enter a value for this secret. Paste the PEM private key; ctrl+j or alt+enter inserts a newline.",
		"",
	)
	if index := document.fieldIndexByID(initReviewerEntityFieldGitHubAppPrivateKey); index >= 0 {
		(*document)[index].Hidden = hidden || kind != initReviewerEntityKindGitHubApp
	}
}

func initReviewerEntitySetCredentialFieldsHidden(model *initLinearEditorModel, kind initReviewerEntityKind, hideDetails bool) {
	hidePAT := hideDetails || kind != initReviewerEntityKindPAT
	hideGitHubApp := hideDetails || kind != initReviewerEntityKindGitHubApp
	model.setFieldHidden(initReviewerEntityFieldCredentialValues, hideDetails)
	model.setFieldHidden(initReviewerEntityFieldGitToken, hidePAT)
	model.setFieldHidden(initReviewerEntityFieldGitHubAppPrivateKey, hideGitHubApp)
}

func initReviewerEntitySetGitHubAppConfigFieldsHidden(model *initLinearEditorModel, kind initReviewerEntityKind, hideDetails bool) {
	hideGitHubApp := hideDetails || kind != initReviewerEntityKindGitHubApp
	model.setFieldHidden(initReviewerEntityFieldGitHubAppDetails, hideGitHubApp)
	model.setFieldHidden(initReviewerEntityFieldGitHubAppID, hideGitHubApp)
	if hideGitHubApp {
		model.setFieldValue(initReviewerEntityFieldGitHubAppID, "")
	}
}

func initReviewerEntityClearCredentialFieldValues(model *initLinearEditorModel) {
	for _, id := range []initLinearFieldID{
		initReviewerEntityFieldGitToken,
		initReviewerEntityFieldGitHubAppPrivateKey,
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
	case initReviewerEntityFieldGitHubAppPrivateKey:
		return credentials.GitHubAppPrivateKeyKey
	default:
		return ""
	}
}

func initReviewerEntityCredentialFieldID(key string) initLinearFieldID {
	switch key {
	case credentials.GitTokenKey:
		return initReviewerEntityFieldGitToken
	case credentials.GitHubAppPrivateKeyKey:
		return initReviewerEntityFieldGitHubAppPrivateKey
	default:
		return ""
	}
}

func initReviewerCredentialStatusWithDocumentWrites(status initReviewerCredentialStatus, document initLinearDocument) initReviewerCredentialStatus {
	return initCredentialStatusWithDocumentWrites(status, document, initReviewerEntityCredentialFieldID)
}

func initCredentialStatusWithDocumentWrites(status initReviewerCredentialStatus, document initLinearDocument, fieldIDForKey func(string) initLinearFieldID) initReviewerCredentialStatus {
	status.Keys = append([]initReviewerCredentialKeyStatus(nil), status.Keys...)
	for index, key := range status.Keys {
		fieldID := fieldIDForKey(key.Key)
		if fieldID == "" || document.fieldHidden(fieldID) {
			continue
		}
		if strings.TrimSpace(credentials.TrimSecretIngress(document.fieldValue(fieldID))) == "" {
			continue
		}
		if status.Keys[index].State == initReviewerCredentialKeyExisting {
			status.Keys[index].State = initReviewerCredentialKeyReplacement
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
		if ctx.ProbeCredentialStatus != nil {
			if probed, ok := ctx.ProbeCredentialStatus(status.Ref); ok {
				status = probed
				return status, true
			}
		}
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
	return missingInitCredentialKeys(status, reviewerEntityKeepsCurrentCredentialRef(state, document))
}

func missingInitCredentialKeys(status initReviewerCredentialStatus, keepsCurrentRef bool) []string {
	var missing []string
	for _, key := range status.Keys {
		if !key.Required {
			continue
		}
		switch key.State {
		case initReviewerCredentialKeyExisting, initReviewerCredentialKeyStaged, initReviewerCredentialKeyReplacement:
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
	if len(writes) > 0 && reviewerEntityKeepsCurrentCredentialRef(state, document) {
		overwrite = true
	}
	editDraft.ReviewerCredentialWriteRef = ref
	editDraft.ReviewerCredentialStore = initCredentialStoreDraftValue(status.Ref.Store)
	editDraft.ReviewerCredentialWriteStore = status.SecretsProfile
	editDraft.ReviewerCredentialWrites = writes
	editDraft.ReviewerCredentialOverwrite = overwrite
	editDraft.ReviewerCredentialSatisfied = reviewerEntityPreservesExistingCredentialRefWithoutWrites(state, document) || len(missingReviewerEntityInlineCredentialKeys(state, status, document)) == 0
}

func reviewerEntityCredentialWritesFromDocument(status initReviewerCredentialStatus, document initLinearDocument) (map[string]string, bool) {
	return initCredentialWritesFromDocument(status, document, initReviewerEntityCredentialFieldID)
}

func initCredentialWritesFromDocument(status initReviewerCredentialStatus, document initLinearDocument, fieldIDForKey func(string) initLinearFieldID) (map[string]string, bool) {
	writes := map[string]string{}
	for _, key := range status.Keys {
		fieldID := fieldIDForKey(key.Key)
		if fieldID == "" || document.fieldHidden(fieldID) {
			continue
		}
		value := credentials.TrimSecretIngress(document.fieldValue(fieldID))
		if strings.TrimSpace(value) == "" {
			continue
		}
		writes[key.Key] = value
	}
	return writes, initCredentialWriteRequiresOverwrite(status, writes)
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
	if _, restore := initLinearRestoreSelectionName("reviewer_entity", selection); restore {
		hideDetails = true
		if detailsIndex >= 0 {
			model.document[detailsIndex].Description = "This reviewer entity is staged for deletion. Restore it to make it available again."
		}
	}
	model.setFieldHidden(initReviewerEntityFieldLabel, hideDetails)
	model.setFieldHidden(initReviewerEntityFieldCredentialStore, hideDetails)
	model.setFieldHidden(initReviewerEntityFieldSecretLocation, hideDetails)
	model.setFieldHidden(initReviewerEntityFieldCredentialStatus, hideDetails)
	initReviewerEntitySetGitHubAppConfigFieldsHidden(model, state.kind, hideDetails)
	initReviewerEntitySetCredentialFieldsHidden(model, state.kind, hideDetails)
	if !hideDetails {
		model.setFieldTitle(initReviewerEntityFieldSecretLocation, reviewerEntitySecretLocationTitle(state.kind))
		model.setFieldTitle(initReviewerEntityFieldCredentialStatus, reviewerEntityCredentialStatusTitle(state.kind))
		model.setFieldDescription(initReviewerEntityFieldSecretLocation, reviewerEntitySecretLocationDescription(state.kind))
	}
	if resetDetails && !hideDetails {
		labelInput, reviewerSecretLocation := state.fieldSeeds()
		model.setFieldValue(initReviewerEntityFieldLabel, labelInput)
		model.setFieldValue(initReviewerEntityFieldGitHubAppID, strings.TrimSpace(state.seed.ReviewerGitHubAppID))
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

func reviewerEntityGitHubAppIDFromDocument(kind initReviewerEntityKind, document initLinearDocument) (string, error) {
	if kind != initReviewerEntityKindGitHubApp {
		return "", nil
	}
	appID := strings.TrimSpace(document.fieldValue(initReviewerEntityFieldGitHubAppID))
	if appID == "" {
		return "", fmt.Errorf("GitHub App ID is required")
	}
	if err := validateOptionalDecimalID("GitHub App ID")(appID); err != nil {
		return "", err
	}
	return appID, nil
}

func initReviewerEntityRefreshCredentialStatus(model *initLinearEditorModel, ctx initPromptContext, seed initDraft, state reviewerEntityEditorState) {
	if status, ok := reviewerCredentialStatusForDocument(ctx, seed, state, model.document); ok {
		model.setFieldDescription(initReviewerEntityFieldCredentialStatus, initReviewerCredentialStatusDescription(status))
		model.setFieldDescription(
			initReviewerEntityFieldGitToken,
			initSecretFieldDescription(
				initReviewerCredentialStatusKeyState(status, credentials.GitTokenKey),
				"Enter a value for this secret.",
			),
		)
		model.setFieldDescription(
			initReviewerEntityFieldGitHubAppPrivateKey,
			initSecretFieldDescription(
				initReviewerCredentialStatusKeyState(status, credentials.GitHubAppPrivateKeyKey),
				"Enter a value for this secret. Paste the PEM private key; ctrl+j or alt+enter inserts a newline.",
			),
		)
	}
}

func initReviewerEntitySetSelectionOptions(model *initLinearEditorModel, ctx initPromptContext, selected string) {
	initLinearSetSelectionOptions(model, initReviewerEntityFieldSelection, initReviewerEntityLinearSelectionOptions(ctx), selected,
		func(value string) bool { _, ok := ctx.ReviewerEntities[value]; return ok },
		func(value string) bool { _, ok := initLinearRestoreSelectionName("reviewer_entity", value); return ok },
	)
}

func reviewerEntityEditorStateForSelection(ctx initPromptContext, seed initDraft, selection string) (reviewerEntityEditorState, error) {
	if entity, ok := ctx.ReviewerEntities[selection]; ok {
		candidate := seed
		applyReviewerEntityInventorySelection(&candidate, selection, ctx.ReviewerEntities)
		return newReviewerEntityEditorState(entity, candidate, true, ctx.StandaloneReviewerEntityMode)
	}
	if _, restore := initLinearRestoreSelectionName("reviewer_entity", selection); restore {
		return newReviewerEntityEditorState(initReviewerEntityDraft{Kind: initReviewerEntityKindUseGitIdentity}, seed, false, ctx.StandaloneReviewerEntityMode)
	}
	candidate := seed
	applyReviewerEntityInventorySelection(&candidate, selection, ctx.ReviewerEntities)
	return newReviewerEntityEditorState(initReviewerEntityDraft{Kind: initReviewerEntityKind(selection)}, candidate, false, ctx.StandaloneReviewerEntityMode)
}

func reviewerEntityKindDetailDescription(kind initReviewerEntityKind) string {
	label := reviewerEntityKindDetailLabel(kind)
	if kind != initReviewerEntityKindGitHubApp {
		return label
	}
	return label + ". GitHub App ID is saved in config.yml; required credential key: " + credentials.GitHubAppPrivateKeyKey + "."
}

func reviewerEntitySecretLocationTitle(kind initReviewerEntityKind) string {
	if kind == initReviewerEntityKindGitHubApp {
		return "Reviewer private-key location"
	}
	return "Reviewer secret location"
}

func reviewerEntityCredentialStatusTitle(kind initReviewerEntityKind) string {
	if kind == initReviewerEntityKindGitHubApp {
		return "Reviewer private-key status"
	}
	return "Reviewer credential status"
}

func reviewerEntitySecretLocationDescription(kind initReviewerEntityKind) string {
	if kind == initReviewerEntityKindPAT {
		base := "Credential name for this reviewer. This is where cr stores the PAT in the selected credential store. Change it only if you need a custom credential-store location."
		return base + " Store required secret " + credentials.GitTokenKey + " at this name."
	}
	if kind == initReviewerEntityKindGitHubApp {
		return "Credential name for this reviewer's GitHub App private key. Change it only if you need a custom credential-store location. Store required secret " + credentials.GitHubAppPrivateKeyKey + " at this name."
	}
	return "Credential name for this reviewer. Change it only if you need a custom credential-store location."
}

func initReviewerEntityDraftFromDocument(ctx initPromptContext, seed initDraft, document initLinearDocument) (initDraft, error) {
	selection := document.selectedValue(initReviewerEntityFieldSelection)
	state, err := reviewerEntityEditorStateForSelection(ctx, seed, selection)
	if err != nil {
		return initDraft{}, err
	}
	return state.draftFromDocument(ctx, document)
}

package credentialcmd

import (
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/open-cli-collective/cli-common/credstore"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
)

type initRepositoryAccessEditorRunner func(initLinearEditor, io.Reader, io.Writer) (initLinearEditorModel, error)

const (
	initRepositoryAccessFieldSelection           initLinearFieldID = "repository_access_selection"
	initRepositoryAccessFieldName                initLinearFieldID = "repository_access_name"
	initRepositoryAccessFieldHost                initLinearFieldID = "repository_access_host"
	initRepositoryAccessFieldAuth                initLinearFieldID = "repository_access_auth"
	initRepositoryAccessFieldGitHubAppID         initLinearFieldID = "repository_access_github_app_id"
	initRepositoryAccessFieldCredentialStore     initLinearFieldID = "repository_access_credential_store"
	initRepositoryAccessFieldCredentialName      initLinearFieldID = "repository_access_credential_name"
	initRepositoryAccessFieldCredentialStatus    initLinearFieldID = "repository_access_credential_status"
	initRepositoryAccessFieldCredentialValues    initLinearFieldID = "repository_access_credential_values"
	initRepositoryAccessFieldGitToken            initLinearFieldID = "repository_access_git_token"
	initRepositoryAccessFieldGitHubAppPrivateKey initLinearFieldID = "repository_access_github_app_private_key"
	initRepositoryAccessFieldAction              initLinearFieldID = "repository_access_action"
)

const initConfigureNewRepositoryAccessSelection = "__configure_new_repository_access__"

const initRepositoryAccessRestoreSelectionPrefix = "__restore_repository_access__:"

var initRepositoryAccessGitConfigValue = func(key string) string {
	out, err := exec.Command("git", "config", "--get", key).Output() // #nosec G204 -- callers pass fixed git config keys only.
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (p huhInitRepositoryAccessPrompter) EditRepositoryAccess(prompt initRepositoryAccessPrompt) (initDraft, error) {
	seed := seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.ExistingProfile)
	editor := initRepositoryAccessLinearEditor(prompt.Context, seed)
	model, err := p.runRepositoryAccessEditor(editor)
	if err != nil {
		return initDraft{}, err
	}
	switch model.resultAction {
	case initDetailActionEdit:
		return initRepositoryAccessDraftFromDocument(prompt.Context, seed, model.document), nil
	case initLinearResultActionDelete:
		selection := model.document.selectedValue(initRepositoryAccessFieldSelection)
		return initDraft{
			Action:       initDraftActionDeleteRepositoryAccess,
			ActionTarget: selection,
		}, nil
	case initLinearResultActionRestore:
		selection := model.document.selectedValue(initRepositoryAccessFieldSelection)
		name, _ := initRepositoryAccessRestoreSelectionName(selection)
		return initDraft{
			Action:       initDraftActionUndoDeleteRepositoryAccess,
			ActionTarget: name,
		}, nil
	default:
		return initDraft{}, errInitNavigateBack
	}
}

func (p huhInitRepositoryAccessPrompter) runRepositoryAccessEditor(editor initLinearEditor) (initLinearEditorModel, error) {
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
		return initLinearEditorModel{}, fmt.Errorf("repository access editor returned %T", finalModel)
	}
	return model, nil
}

func initRepositoryAccessLinearEditor(ctx initPromptContext, _ initDraft) initLinearEditor {
	options := initRepositoryAccessSelectionOptionsForContext(ctx)
	selection := initRepositoryAccessDefaultSelection(ctx, options)
	scope := initRepositoryAccessScopeForSelection(ctx, selection)

	var document initLinearDocument
	document.addSection("Repository access", "Configure how cr accesses Git hosts as you. Review profiles select one repository access entry.")
	document.addEditableSelect(initRepositoryAccessFieldSelection, "Repository access", "", options, selection)
	document.addEditableInput(initRepositoryAccessFieldName, "Repository access name", "Stable name for this repository access entry.", scope.Name, validateRequiredText("repository access name is required"))
	document.addEditableInput(initRepositoryAccessFieldHost, "Git host", "Git host this access entry applies to, such as github.com or github.mycompany.com.", scope.Host, validateRequiredText("git host is required"))
	document.addEditableSelect(initRepositoryAccessFieldAuth, "Git auth mode", "", []huh.Option[string]{
		huh.NewOption("Personal access token", string(config.GitAuthModePAT)),
		huh.NewOption("GitHub App", string(config.GitAuthModeGitHubApp)),
	}, string(scope.AuthMode))
	document.addEditableInput(initRepositoryAccessFieldGitHubAppID, "GitHub App ID", "Numeric GitHub App ID. This is not a secret and is saved in config.yml.", scope.GitHubAppID, validateOptionalDecimalID("GitHub App ID"), initLinearFieldOptions{Hidden: scope.AuthMode != config.GitAuthModeGitHubApp})
	document.addEditableSelect(initRepositoryAccessFieldCredentialStore, "Git credential store", "Where this access entry's Git credential is stored.", initCredentialStoreOptions(ctx.ExistingConfig), initCredentialStoreDraftValue(scope.CredentialStore))
	document.addEditableInput(initRepositoryAccessFieldCredentialName, "Git credential name", "Full credential name under the selected store.", scope.CredentialRef, validateRequiredCredentialRef)
	document.addSectionField(initRepositoryAccessFieldCredentialStatus, "Git credential status", "")
	document.addSectionField(initRepositoryAccessFieldCredentialValues, "Git credential value", "Enter the required Git credential here. Values are hidden, staged in memory, and written only when you Commit staged changes and exit.")
	document.addEditableSecretInput(initRepositoryAccessFieldGitToken, "GitHub PAT", "Enter a value for this secret.", "", nil)
	document.addEditableSecretTextarea(initRepositoryAccessFieldGitHubAppPrivateKey, "GitHub App private key", "Enter a value for this secret. Paste the PEM private key; ctrl+j or alt+enter inserts a newline.", "")
	document.addEditableSelect(initRepositoryAccessFieldAction, "Repository access action", "", []huh.Option[string]{
		huh.NewOption("Stage repository access settings", initDetailActionEdit),
		huh.NewOption("Back without staging", initDetailActionBack),
	}, initDetailActionEdit)

	editor := initLinearEditor{
		Document: document,
		OnFieldChange: func(model *initLinearEditorModel, index int) {
			if index < 0 || index >= len(model.document) {
				return
			}
			//nolint:exhaustive // This repository-access editor reacts only to fields that affect derived repository-access state.
			switch model.document[index].ID {
			case initRepositoryAccessFieldSelection:
				initRepositoryAccessSyncFields(model, ctx)
			case initRepositoryAccessFieldName:
				initRepositoryAccessMaybeUpdateAutoCredentialRef(model)
				initRepositoryAccessRefreshCredentialStatus(model, ctx)
			case initRepositoryAccessFieldCredentialName:
				model.document[index].AutoManaged = false
				initRepositoryAccessRefreshCredentialStatus(model, ctx)
			case initRepositoryAccessFieldCredentialStore:
				initRepositoryAccessRefreshCredentialStatus(model, ctx)
			case initRepositoryAccessFieldAuth:
				initRepositoryAccessSyncGitHubAppField(model)
				initRepositoryAccessSetCredentialFieldsHidden(model)
				initRepositoryAccessClearCredentialFieldValues(model)
				initRepositoryAccessRefreshCredentialStatus(model, ctx)
			case initRepositoryAccessFieldGitToken, initRepositoryAccessFieldGitHubAppPrivateKey:
				initRepositoryAccessRefreshCredentialStatus(model, ctx)
			default:
				return
			}
		},
		OnEnter: func(model *initLinearEditorModel) (bool, tea.Cmd) {
			if model.focused < 0 || model.focused >= len(model.document) {
				return false, nil
			}
			if model.document[model.focused].ID != initRepositoryAccessFieldAction {
				return false, nil
			}
			switch model.document.selectedValue(initRepositoryAccessFieldAction) {
			case initDetailActionBack:
				model.resultAction = initDetailActionBack
				return true, tea.Quit
			case initDetailActionEdit:
				if err := validateRepositoryAccessDocument(ctx, model.document); err != nil {
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
	initRepositoryAccessSyncFields(&model, ctx)
	editor.Document = model.document
	return editor
}

func initRepositoryAccessSelectionOptions(scopes map[string]initGitScopeDraft) []huh.Option[string] {
	names := make([]string, 0, len(scopes))
	for name := range scopes {
		names = append(names, name)
	}
	sort.Strings(names)
	options := make([]huh.Option[string], 0, len(names)+1)
	for _, name := range names {
		options = append(options, huh.NewOption(initGitScopeLabel(scopes[name]), name))
	}
	options = append(options, huh.NewOption("Configure new repository access", initConfigureNewRepositoryAccessSelection))
	return options
}

func initRepositoryAccessSelectionOptionsForContext(ctx initPromptContext) []huh.Option[string] {
	options := initRepositoryAccessSelectionOptions(ctx.GitScopes)
	pendingNames := make([]string, 0, len(ctx.PendingRepositoryAccessDeletes))
	for name := range ctx.PendingRepositoryAccessDeletes {
		pendingNames = append(pendingNames, name)
	}
	sort.Strings(pendingNames)
	for _, name := range pendingNames {
		options = append(options, huh.NewOption(repositoryAccessDeletePendingLabel(name), initRepositoryAccessRestoreSelectionPrefix+name))
	}
	return dedupeInitStringOptions(options)
}

func initRepositoryAccessDefaultSelection(ctx initPromptContext, options []huh.Option[string]) string {
	if ctx.ExistingProfileName != "" {
		if selected := strings.TrimSpace(ctx.ProfileGitScopes[ctx.ExistingProfileName]); selected != "" {
			return normalizeInitStringSelectionValue(selected, options)
		}
	}
	return normalizeInitStringSelectionValue(initConfigureNewRepositoryAccessSelection, options)
}

func initRepositoryAccessScopeForSelection(ctx initPromptContext, selection string) initGitScopeDraft {
	if selection != initConfigureNewRepositoryAccessSelection {
		if scope, ok := ctx.GitScopes[selection]; ok {
			scope.Name = selection
			return scope
		}
	}
	name := uniqueInitGitScopeName(ctx.GitScopes, initGitScopeDraft{
		Host:     "github.com",
		AuthMode: config.GitAuthModePAT,
	}.suggestedRepositoryAccessName())
	return initGitScopeDraft{
		Name:            name,
		Host:            "github.com",
		AuthMode:        config.GitAuthModePAT,
		CredentialStore: initCredentialStoreDefaultID(),
		CredentialRef:   initRepositoryAccessCredentialRef(name),
	}
}

func initRepositoryAccessRestoreSelectionName(selection string) (string, bool) {
	if !strings.HasPrefix(selection, initRepositoryAccessRestoreSelectionPrefix) {
		return "", false
	}
	return strings.TrimPrefix(selection, initRepositoryAccessRestoreSelectionPrefix), true
}

func repositoryAccessDeletePendingLabel(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "Repository access (Staged for deletion)"
	}
	return fmt.Sprintf("%s (Staged for deletion)", name)
}

func initRepositoryAccessSyncFields(model *initLinearEditorModel, ctx initPromptContext) {
	selection := model.document.selectedValue(initRepositoryAccessFieldSelection)
	initRepositoryAccessSetSelectionOptions(model, ctx, selection)
	scope := initRepositoryAccessScopeForSelection(ctx, selection)
	hideDetails := false
	if _, restore := initRepositoryAccessRestoreSelectionName(selection); restore {
		hideDetails = true
	}
	model.setFieldValue(initRepositoryAccessFieldName, scope.Name)
	model.setFieldValue(initRepositoryAccessFieldHost, scope.Host)
	model.selectFieldValue(initRepositoryAccessFieldAuth, string(scope.AuthMode))
	model.setFieldValue(initRepositoryAccessFieldGitHubAppID, scope.GitHubAppID)
	model.selectFieldValue(initRepositoryAccessFieldCredentialStore, initCredentialStoreDraftValue(scope.CredentialStore))
	model.setFieldValue(initRepositoryAccessFieldCredentialName, scope.CredentialRef)
	if refIndex := model.document.fieldIndexByID(initRepositoryAccessFieldCredentialName); refIndex >= 0 {
		model.document[refIndex].AutoManaged = initRepositoryAccessCredentialRef(scope.Name) == strings.TrimSpace(scope.CredentialRef)
	}
	initRepositoryAccessSetDetailsHidden(model, hideDetails)
	if hideDetails {
		return
	}
	initRepositoryAccessSyncGitHubAppField(model)
	initRepositoryAccessSetCredentialFieldsHidden(model)
	initRepositoryAccessClearCredentialFieldValues(model)
	initRepositoryAccessRefreshCredentialStatus(model, ctx)
}

func initRepositoryAccessSetSelectionOptions(model *initLinearEditorModel, ctx initPromptContext, selected string) {
	index := model.document.fieldIndexByID(initRepositoryAccessFieldSelection)
	if index < 0 {
		return
	}
	options := initLinearOptionsFromHuh(initRepositoryAccessSelectionOptionsForContext(ctx), selected)
	for optionIndex := range options {
		option := &options[optionIndex]
		if _, configured := ctx.ExistingConfig.RepositoryAccess[option.Value]; configured {
			option.Deletable = true
		}
		if _, restorable := initRepositoryAccessRestoreSelectionName(option.Value); restorable {
			option.Restorable = true
		}
	}
	model.document[index].Options = options
}

func initRepositoryAccessSetDetailsHidden(model *initLinearEditorModel, hidden bool) {
	for _, id := range []initLinearFieldID{
		initRepositoryAccessFieldName,
		initRepositoryAccessFieldHost,
		initRepositoryAccessFieldAuth,
		initRepositoryAccessFieldCredentialStore,
		initRepositoryAccessFieldCredentialName,
		initRepositoryAccessFieldCredentialStatus,
		initRepositoryAccessFieldCredentialValues,
		initRepositoryAccessFieldAction,
	} {
		model.setFieldHidden(id, hidden)
	}
	if hidden {
		for _, id := range []initLinearFieldID{
			initRepositoryAccessFieldGitHubAppID,
			initRepositoryAccessFieldGitToken,
			initRepositoryAccessFieldGitHubAppPrivateKey,
		} {
			model.setFieldHidden(id, true)
		}
	}
}

func initRepositoryAccessMaybeUpdateAutoCredentialRef(model *initLinearEditorModel) {
	refIndex := model.document.fieldIndexByID(initRepositoryAccessFieldCredentialName)
	if refIndex < 0 || !model.document[refIndex].AutoManaged {
		return
	}
	model.setFieldValue(initRepositoryAccessFieldCredentialName, initRepositoryAccessCredentialRef(model.document.fieldValue(initRepositoryAccessFieldName)))
	if refIndex = model.document.fieldIndexByID(initRepositoryAccessFieldCredentialName); refIndex >= 0 {
		model.document[refIndex].AutoManaged = true
	}
}

func initRepositoryAccessCredentialRef(name string) string {
	segment := credstore.EscapeRefSegment(strings.TrimSpace(name))
	ref, err := credentials.FormatRef(segment)
	if err != nil {
		return ""
	}
	return ref
}

func (scope initGitScopeDraft) suggestedRepositoryAccessName() string {
	if config.NormalizeHost(scope.Host) == "github.com" && scope.AuthMode == config.GitAuthModePAT {
		if username := initRepositoryAccessLocalGitHubUsername(); username != "" {
			return "github-" + username
		}
	}
	return scope.suggestedName()
}

func initRepositoryAccessLocalGitHubUsername() string {
	for _, key := range []string{"github.user", "github.username"} {
		if username := normalizeRepositoryAccessNameSegment(initRepositoryAccessGitConfigValue(key)); username != "" {
			return username
		}
	}
	return ""
}

func normalizeRepositoryAccessNameSegment(value string) string {
	segment := credstore.EscapeRefSegment(strings.TrimSpace(value))
	segment = strings.Trim(segment, "-_")
	return strings.ToLower(segment)
}

func initRepositoryAccessSetCredentialFieldsHidden(model *initLinearEditorModel) {
	authMode := config.GitAuthMode(model.document.selectedValue(initRepositoryAccessFieldAuth))
	model.setFieldHidden(initRepositoryAccessFieldGitToken, authMode != config.GitAuthModePAT)
	model.setFieldHidden(initRepositoryAccessFieldGitHubAppPrivateKey, authMode != config.GitAuthModeGitHubApp)
}

func initRepositoryAccessClearCredentialFieldValues(model *initLinearEditorModel) {
	for _, id := range []initLinearFieldID{
		initRepositoryAccessFieldGitToken,
		initRepositoryAccessFieldGitHubAppPrivateKey,
	} {
		model.setFieldValue(id, "")
	}
}

func initRepositoryAccessCredentialFieldID(key string) initLinearFieldID {
	switch key {
	case credentials.GitTokenKey:
		return initRepositoryAccessFieldGitToken
	case credentials.GitHubAppPrivateKeyKey:
		return initRepositoryAccessFieldGitHubAppPrivateKey
	default:
		return ""
	}
}

func initRepositoryAccessRefreshCredentialStatus(model *initLinearEditorModel, ctx initPromptContext) {
	if status, ok := repositoryAccessCredentialStatusForDocument(ctx, model.document); ok {
		model.setFieldDescription(initRepositoryAccessFieldCredentialStatus, initRepositoryAccessCredentialStatusDescription(status))
		model.setFieldDescription(
			initRepositoryAccessFieldGitToken,
			initSecretFieldDescription(
				initReviewerCredentialStatusKeyState(status, credentials.GitTokenKey),
				"Enter a value for this secret.",
			),
		)
		model.setFieldDescription(
			initRepositoryAccessFieldGitHubAppPrivateKey,
			initSecretFieldDescription(
				initReviewerCredentialStatusKeyState(status, credentials.GitHubAppPrivateKeyKey),
				"Enter a value for this secret. Paste the PEM private key; ctrl+j or alt+enter inserts a newline.",
			),
		)
	}
}

func repositoryAccessCredentialStatusForDocument(ctx initPromptContext, document initLinearDocument) (initReviewerCredentialStatus, bool) {
	status, ok := baseRepositoryAccessCredentialStatusForDocument(ctx, document)
	if !ok {
		return initReviewerCredentialStatus{}, false
	}
	return repositoryAccessCredentialStatusWithDocumentWrites(status, document), true
}

func baseRepositoryAccessCredentialStatusForDocument(ctx initPromptContext, document initLinearDocument) (initReviewerCredentialStatus, bool) {
	ref := config.CredentialRef{
		Purpose: "git",
		Store:   initCredentialStoreDraftValue(document.selectedValue(initRepositoryAccessFieldCredentialStore)),
		Ref:     strings.TrimSpace(document.fieldValue(initRepositoryAccessFieldCredentialName)),
		Mode:    document.selectedValue(initRepositoryAccessFieldAuth),
	}
	if ref.Ref == "" {
		return initReviewerCredentialStatus{}, false
	}
	for _, status := range ctx.RepositoryAccessCredentialStatuses {
		if status.Ref.Purpose == ref.Purpose &&
			initCredentialStoreDraftValue(status.Ref.Store) == initCredentialStoreDraftValue(ref.Store) &&
			status.Ref.Ref == ref.Ref &&
			status.Ref.Mode == ref.Mode {
			return status, true
		}
	}
	if ctx.RepositoryAccessCredentialStatuses != nil {
		if ctx.ProbeCredentialStatus != nil {
			if status, ok := ctx.ProbeCredentialStatus(ref); ok {
				return status, true
			}
		}
	}
	if initReviewerCredentialStatusListContainsUnavailable(ctx.RepositoryAccessCredentialStatuses) {
		return synthesizeUnavailableReviewerCredentialStatus(ctx, ref), true
	}
	return synthesizeReviewerCredentialStatus(ctx, ref), true
}

func repositoryAccessCredentialStatusWithDocumentWrites(status initReviewerCredentialStatus, document initLinearDocument) initReviewerCredentialStatus {
	status.Keys = append([]initReviewerCredentialKeyStatus(nil), status.Keys...)
	for index, key := range status.Keys {
		fieldID := initRepositoryAccessCredentialFieldID(key.Key)
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

func initRepositoryAccessCredentialStatusDescription(status initReviewerCredentialStatus) string {
	return initReviewerCredentialStatusDescription(status)
}

func missingRepositoryAccessInlineCredentialKeys(ctx initPromptContext, status initReviewerCredentialStatus, document initLinearDocument) []string {
	var missing []string
	keepsCurrentRef := repositoryAccessKeepsCurrentCredentialRef(ctx, document)
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

func repositoryAccessKeepsCurrentCredentialRef(ctx initPromptContext, document initLinearDocument) bool {
	selection := document.selectedValue(initRepositoryAccessFieldSelection)
	if selection == "" || selection == initConfigureNewRepositoryAccessSelection {
		return false
	}
	scope, ok := ctx.GitScopes[selection]
	if !ok {
		return false
	}
	return initCredentialStoreDraftValue(document.selectedValue(initRepositoryAccessFieldCredentialStore)) == initCredentialStoreDraftValue(scope.CredentialStore) &&
		strings.TrimSpace(document.fieldValue(initRepositoryAccessFieldCredentialName)) == strings.TrimSpace(scope.CredentialRef) &&
		document.selectedValue(initRepositoryAccessFieldAuth) == string(scope.AuthMode)
}

func repositoryAccessCredentialWritesFromDocument(status initReviewerCredentialStatus, document initLinearDocument) (map[string]string, bool) {
	writes := map[string]string{}
	for _, key := range status.Keys {
		fieldID := initRepositoryAccessCredentialFieldID(key.Key)
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

func initRepositoryAccessSyncGitHubAppField(model *initLinearEditorModel) {
	authMode := config.GitAuthMode(model.document.selectedValue(initRepositoryAccessFieldAuth))
	hidden := authMode != config.GitAuthModeGitHubApp
	model.setFieldHidden(initRepositoryAccessFieldGitHubAppID, hidden)
	if hidden {
		model.setFieldValue(initRepositoryAccessFieldGitHubAppID, "")
	}
}

func validateRepositoryAccessDocument(ctx initPromptContext, document initLinearDocument) error {
	if err := validateRequiredText("repository access name is required")(document.fieldValue(initRepositoryAccessFieldName)); err != nil {
		return err
	}
	if err := validateRequiredText("git host is required")(document.fieldValue(initRepositoryAccessFieldHost)); err != nil {
		return err
	}
	if config.GitAuthMode(document.selectedValue(initRepositoryAccessFieldAuth)) == config.GitAuthModeGitHubApp {
		appID := strings.TrimSpace(document.fieldValue(initRepositoryAccessFieldGitHubAppID))
		if appID == "" {
			return fmt.Errorf("GitHub App ID is required")
		}
		if err := validateOptionalDecimalID("GitHub App ID")(appID); err != nil {
			return err
		}
	}
	if err := validateRequiredCredentialRef(document.fieldValue(initRepositoryAccessFieldCredentialName)); err != nil {
		return err
	}
	status, ok := repositoryAccessCredentialStatusForDocument(ctx, document)
	if !ok {
		return fmt.Errorf("git credential status unavailable")
	}
	if missing := missingRepositoryAccessInlineCredentialKeys(ctx, status, document); len(missing) > 0 {
		return fmt.Errorf("set repository access credentials before staging: %s", strings.Join(missing, ", "))
	}
	return nil
}

func initRepositoryAccessDraftFromDocument(ctx initPromptContext, seed initDraft, document initLinearDocument) initDraft {
	draft := seed
	draft.RepositoryAccessName = strings.TrimSpace(document.fieldValue(initRepositoryAccessFieldName))
	draft.GitHost = strings.TrimSpace(document.fieldValue(initRepositoryAccessFieldHost))
	draft.GitAuth = document.selectedValue(initRepositoryAccessFieldAuth)
	draft.GitHubAppID = strings.TrimSpace(document.fieldValue(initRepositoryAccessFieldGitHubAppID))
	draft.GitCredentialStore = initCredentialStoreDraftValue(document.selectedValue(initRepositoryAccessFieldCredentialStore))
	draft.GitCredentialRef = strings.TrimSpace(document.fieldValue(initRepositoryAccessFieldCredentialName))
	applyRepositoryAccessCredentialDraftFromDocument(&draft, ctx, document)
	selection := document.selectedValue(initRepositoryAccessFieldSelection)
	if selection != "" && selection != initConfigureNewRepositoryAccessSelection {
		draft.ActionTarget = selection
	}
	return draft
}

func applyRepositoryAccessCredentialDraftFromDocument(draft *initDraft, ctx initPromptContext, document initLinearDocument) {
	originalStatus, ok := baseRepositoryAccessCredentialStatusForDocument(ctx, document)
	if !ok {
		return
	}
	status := repositoryAccessCredentialStatusWithDocumentWrites(originalStatus, document)
	ref := strings.TrimSpace(status.Ref.Ref)
	if ref == "" {
		return
	}
	writes, overwrite := repositoryAccessCredentialWritesFromDocument(originalStatus, document)
	draft.GitCredentialWriteRef = ref
	draft.GitCredentialStore = initCredentialStoreDraftValue(status.Ref.Store)
	draft.GitCredentialWriteStore = status.SecretsProfile
	draft.GitCredentialWrites = writes
	draft.GitCredentialOverwrite = overwrite
	draft.GitCredentialSatisfied = repositoryAccessKeepsCurrentCredentialRef(ctx, document) || len(missingRepositoryAccessInlineCredentialKeys(ctx, status, document)) == 0
}

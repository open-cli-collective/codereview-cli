package credentialcmd

import (
	"fmt"
	"io"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
)

type initRepositoryAccessEditorRunner func(initLinearEditor, io.Reader, io.Writer) (initLinearEditorModel, error)

const (
	initRepositoryAccessFieldSelection       initLinearFieldID = "repository_access_selection"
	initRepositoryAccessFieldName            initLinearFieldID = "repository_access_name"
	initRepositoryAccessFieldHost            initLinearFieldID = "repository_access_host"
	initRepositoryAccessFieldAuth            initLinearFieldID = "repository_access_auth"
	initRepositoryAccessFieldGitHubAppID     initLinearFieldID = "repository_access_github_app_id"
	initRepositoryAccessFieldCredentialStore initLinearFieldID = "repository_access_credential_store"
	initRepositoryAccessFieldCredentialName  initLinearFieldID = "repository_access_credential_name"
	initRepositoryAccessFieldAction          initLinearFieldID = "repository_access_action"
)

const initConfigureNewRepositoryAccessSelection = "__configure_new_repository_access__"

func (p huhInitRepositoryAccessPrompter) EditRepositoryAccess(prompt initRepositoryAccessPrompt) (initDraft, error) {
	seed := seedInteractiveInitDraft(prompt.Context.RequestedProfileName, prompt.Context.ExistingProfileName, prompt.Context.ExistingProfile)
	editor := initRepositoryAccessLinearEditor(prompt.Context, seed)
	model, err := p.runRepositoryAccessEditor(editor)
	if err != nil {
		return initDraft{}, err
	}
	switch model.resultAction {
	case initDetailActionEdit:
		return initRepositoryAccessDraftFromDocument(seed, model.document), nil
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

func initRepositoryAccessLinearEditor(ctx initPromptContext, seed initDraft) initLinearEditor {
	options := initRepositoryAccessSelectionOptions(ctx.GitScopes)
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
			switch model.document[index].ID {
			case initRepositoryAccessFieldSelection:
				initRepositoryAccessSyncFields(model, ctx)
			case initRepositoryAccessFieldAuth:
				initRepositoryAccessSyncGitHubAppField(model)
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
				if err := validateRepositoryAccessDocument(model.document); err != nil {
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
	}.suggestedName())
	ref, _ := credentials.FormatRef(name)
	return initGitScopeDraft{
		Name:            name,
		Host:            "github.com",
		AuthMode:        config.GitAuthModePAT,
		CredentialStore: initCredentialStoreDefaultID(),
		CredentialRef:   ref,
	}
}

func initRepositoryAccessSyncFields(model *initLinearEditorModel, ctx initPromptContext) {
	selection := model.document.selectedValue(initRepositoryAccessFieldSelection)
	scope := initRepositoryAccessScopeForSelection(ctx, selection)
	model.setFieldValue(initRepositoryAccessFieldName, scope.Name)
	model.setFieldValue(initRepositoryAccessFieldHost, scope.Host)
	model.selectFieldValue(initRepositoryAccessFieldAuth, string(scope.AuthMode))
	model.setFieldValue(initRepositoryAccessFieldGitHubAppID, scope.GitHubAppID)
	model.selectFieldValue(initRepositoryAccessFieldCredentialStore, initCredentialStoreDraftValue(scope.CredentialStore))
	model.setFieldValue(initRepositoryAccessFieldCredentialName, scope.CredentialRef)
	initRepositoryAccessSyncGitHubAppField(model)
}

func initRepositoryAccessSyncGitHubAppField(model *initLinearEditorModel) {
	authMode := config.GitAuthMode(model.document.selectedValue(initRepositoryAccessFieldAuth))
	hidden := authMode != config.GitAuthModeGitHubApp
	model.setFieldHidden(initRepositoryAccessFieldGitHubAppID, hidden)
	if hidden {
		model.setFieldValue(initRepositoryAccessFieldGitHubAppID, "")
	}
}

func validateRepositoryAccessDocument(document initLinearDocument) error {
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
	return validateRequiredCredentialRef(document.fieldValue(initRepositoryAccessFieldCredentialName))
}

func initRepositoryAccessDraftFromDocument(seed initDraft, document initLinearDocument) initDraft {
	draft := seed
	draft.RepositoryAccessName = strings.TrimSpace(document.fieldValue(initRepositoryAccessFieldName))
	draft.GitHost = strings.TrimSpace(document.fieldValue(initRepositoryAccessFieldHost))
	draft.GitAuth = document.selectedValue(initRepositoryAccessFieldAuth)
	draft.GitHubAppID = strings.TrimSpace(document.fieldValue(initRepositoryAccessFieldGitHubAppID))
	draft.GitCredentialStore = initCredentialStoreDraftValue(document.selectedValue(initRepositoryAccessFieldCredentialStore))
	draft.GitCredentialRef = strings.TrimSpace(document.fieldValue(initRepositoryAccessFieldCredentialName))
	selection := document.selectedValue(initRepositoryAccessFieldSelection)
	if selection != "" && selection != initConfigureNewRepositoryAccessSelection {
		draft.ActionTarget = selection
	}
	return draft
}

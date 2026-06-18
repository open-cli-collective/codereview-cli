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
	"github.com/open-cli-collective/codereview-cli/internal/configedit"
)

type initSecretsManagementEditorRunner func(initLinearEditor, io.Reader, io.Writer) (initLinearEditorModel, error)

const (
	initSecretsManagementFieldTarget           initLinearFieldID = "secrets_management_target"
	initSecretsManagementFieldLegacyBackend    initLinearFieldID = "secrets_management_legacy_backend"
	initSecretsManagementFieldLabel            initLinearFieldID = "secrets_management_label"
	initSecretsManagementFieldBackend          initLinearFieldID = "secrets_management_backend"
	initSecretsManagementFieldVaultID          initLinearFieldID = "secrets_management_1password_vault_id"
	initSecretsManagementFieldTimeout          initLinearFieldID = "secrets_management_1password_timeout"
	initSecretsManagementFieldItemTitlePrefix  initLinearFieldID = "secrets_management_1password_item_title_prefix"
	initSecretsManagementFieldItemTag          initLinearFieldID = "secrets_management_1password_item_tag"
	initSecretsManagementFieldItemFieldTitle   initLinearFieldID = "secrets_management_1password_item_field_title"
	initSecretsManagementFieldConnectHost      initLinearFieldID = "secrets_management_1password_connect_host"
	initSecretsManagementFieldConnectTokenEnv  initLinearFieldID = "secrets_management_1password_connect_token_env"
	initSecretsManagementFieldServiceTokenEnv  initLinearFieldID = "secrets_management_1password_service_token_env"
	initSecretsManagementFieldDesktopAccountID initLinearFieldID = "secrets_management_1password_desktop_account_id"
	initSecretsManagementFieldDefault          initLinearFieldID = "secrets_management_default"
	initSecretsManagementFieldAction           initLinearFieldID = "secrets_management_action"
	initSecretsManagementSectionLegacy         initLinearFieldID = "secrets_management_section_legacy"
	initSecretsManagementSectionProfile        initLinearFieldID = "secrets_management_section_profile"
	initSecretsManagementSectionOnePassword    initLinearFieldID = "secrets_management_section_1password"
	initSecretsManagementSectionConnect        initLinearFieldID = "secrets_management_section_connect"
	initSecretsManagementSectionServiceAccount initLinearFieldID = "secrets_management_section_service_account"
	initSecretsManagementSectionDesktop        initLinearFieldID = "secrets_management_section_desktop"
	initSecretsManagementDefaultNo             string            = "no"
	initSecretsManagementDefaultYes            string            = "yes"
	initSecretsManagementActionDelete          string            = "delete"
)

const initSecretsManagementRestoreSelectionPrefix = "__restore_secrets_management__:"

type initPendingSecretsManagementDelete struct {
	ID      string
	Profile config.SecretsProfile
}

func (p huhInitKeyringBackendPrompter) editKeyringBackendLinear(prompt initKeyringBackendPrompt) (initKeyringBackendEdit, error) {
	working := cloneInitConfigFile(prompt.Config)
	pendingDeletes := map[string]initPendingSecretsManagementDelete{}
	pendingDeleteOrder := []string{}
	for {
		editor := initSecretsManagementLinearEditorWithPendingOrder(working, pendingDeletes, pendingDeleteOrder)
		model, err := p.runSecretsManagementEditor(editor)
		if err != nil {
			return initKeyringBackendEdit{}, err
		}
		switch model.resultAction {
		case initDetailActionEdit:
			return initSecretsManagementEditFromDocument(working, model.document)
		case initLinearResultActionDelete:
			selection := model.document.selectedValue(initSecretsManagementFieldTarget)
			profile, ok := working.Secrets.Profiles[selection]
			if !ok {
				return initKeyringBackendEdit{}, fmt.Errorf("%w: %s", config.ErrSecretsProfileNotFound, selection)
			}
			nextCfg, _, err := configedit.RemoveSecretsProfile(working, selection)
			if err != nil {
				return initKeyringBackendEdit{}, err
			}
			pendingDeletes[selection] = initPendingSecretsManagementDelete{ID: selection, Profile: profile}
			pendingDeleteOrder = appendInitSecretsManagementPendingDeleteOrder(pendingDeleteOrder, selection)
			working = nextCfg
		case initLinearResultActionRestore:
			selection := model.document.selectedValue(initSecretsManagementFieldTarget)
			id, ok := initSecretsManagementRestoreSelectionName(selection)
			if !ok {
				continue
			}
			pending, ok := pendingDeletes[id]
			if !ok {
				continue
			}
			patch := configedit.SecretsProfilePatch{Backend: &pending.Profile.Backend}
			if strings.TrimSpace(pending.Profile.Label) != "" {
				label := pending.Profile.Label
				patch.Label = &label
			}
			nextCfg, _, _, err := configedit.SetSecretsProfile(working, id, patch)
			if err != nil {
				return initKeyringBackendEdit{}, err
			}
			delete(pendingDeletes, id)
			pendingDeleteOrder = removeInitSecretsManagementPendingDeleteOrder(pendingDeleteOrder, id)
			working = nextCfg
		default:
			return initKeyringBackendEdit{}, errInitNavigateBack
		}
	}
}

func (p huhInitKeyringBackendPrompter) runSecretsManagementEditor(editor initLinearEditor) (initLinearEditorModel, error) {
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
		return initLinearEditorModel{}, fmt.Errorf("secrets-management editor returned %T", finalModel)
	}
	return model, nil
}

func initSecretsManagementLinearEditor(cfg config.File) initLinearEditor {
	return initSecretsManagementLinearEditorWithPending(cfg, nil)
}

func initSecretsManagementLinearEditorWithPending(cfg config.File, pendingDeletes map[string]initPendingSecretsManagementDelete) initLinearEditor {
	return initSecretsManagementLinearEditorWithPendingOrder(cfg, pendingDeletes, nil)
}

func initSecretsManagementLinearEditorWithPendingOrder(cfg config.File, pendingDeletes map[string]initPendingSecretsManagementDelete, pendingDeleteOrder []string) initLinearEditor {
	targetOptions := initSecretsManagementTargetOptions(cfg, pendingDeletes, pendingDeleteOrder)
	selectedTarget := normalizeInitStringSelectionValue("", targetOptions)
	var document initLinearDocument
	document.addSection("Secrets management", initSecretsManagementInventoryDescription())
	document.addEditableSelect(initSecretsManagementFieldTarget, "Secrets-management target", "", targetOptions, selectedTarget)
	document.addSectionField(initSecretsManagementSectionLegacy, "Default credential store", "Global fallback credential store used by profiles that do not choose a named secrets-management profile.")
	document.addEditableSelect(initSecretsManagementFieldLegacyBackend, "Legacy persistent backend", "", initLegacySecretsBackendOptions(cfg.Keyring.Backend), strings.TrimSpace(cfg.Keyring.Backend))
	document.addSectionField(initSecretsManagementSectionProfile, "Secrets-management profile", "Secrets-management profiles are reusable credential-store definitions that review profiles can choose later.")
	document.addEditableInput(
		initSecretsManagementFieldLabel,
		"Secrets-management profile label",
		"Choose a human-friendly label for this secrets-management profile. This is what the init menus will show later.",
		"",
		validateOptionalDisplayName,
	)
	document.addEditableSelect(initSecretsManagementFieldBackend, "Secrets-management backend", "", initSecretsProfileBackendOptions(config.SecretsBackendKind(credstore.BackendKeychain)), string(credstore.BackendKeychain))
	document.addSectionField(initSecretsManagementSectionOnePassword, "1Password details", "These are non-secret 1Password settings. Tokens are referenced by environment variable name, not collected here.")
	document.addEditableInput(initSecretsManagementFieldVaultID, "1Password vault name or id", "Required for every 1Password-backed secrets-management profile. Enter a vault name such as Personal, Employee, or My Vault, or enter a stable vault ID. If you have the 1Password CLI installed, `op vault list --format=json` can help inspect available vaults.", "", nil)
	document.addEditableInput(initSecretsManagementFieldItemFieldTitle, "1Password secret name", "Optional name for the field that stores the secret inside each cr-managed 1Password item. Leave blank to use the backend default.", "", validateOptionalDisplayName)
	document.addEditableInput(initSecretsManagementFieldItemTag, "1Password item tag", "Optional tag added to cr-managed 1Password items so the backend can find only the items it owns.", "", validateOptionalDisplayName)
	document.addEditableInput(initSecretsManagementFieldItemTitlePrefix, "1Password item title prefix (advanced)", "Advanced. Prefix for generated 1Password item titles. Leave blank to use the backend default naming convention.", "", validateOptionalDisplayName)
	document.addEditableInput(initSecretsManagementFieldTimeout, "1Password request timeout", "How long cr waits for one 1Password backend request before failing. Leave the default unless your 1Password integration is unusually slow.", "", validateOptionalDuration)
	document.addSectionField(initSecretsManagementSectionConnect, "1Password Connect", "Required only for 1Password Connect profiles.")
	document.addEditableInput(initSecretsManagementFieldConnectHost, "1Password Connect host", "Required only for 1Password Connect profiles.", "", nil)
	document.addEditableInput(initSecretsManagementFieldConnectTokenEnv, "1Password Connect token env var", "Environment variable that holds the 1Password Connect token.", "", validateOptionalDisplayName)
	document.addSectionField(initSecretsManagementSectionServiceAccount, "1Password service account", "Required only for 1Password service-account profiles.")
	document.addEditableInput(initSecretsManagementFieldServiceTokenEnv, "1Password service token env var", "Environment variable that holds the 1Password service-account token.", "", validateOptionalDisplayName)
	document.addSectionField(initSecretsManagementSectionDesktop, "1Password desktop", "Optional desktop integration settings.")
	document.addEditableInput(initSecretsManagementFieldDesktopAccountID, "1Password desktop account id (advanced)", "Advanced. Optional account id when you need to pin this profile to one signed-in 1Password desktop account. Most users should leave this blank.", "", validateOptionalDisplayName)
	document.addEditableSelect(initSecretsManagementFieldDefault, "Default secrets-management profile", "", []huh.Option[string]{
		huh.NewOption("No, keep the current default secrets-management profile", initSecretsManagementDefaultNo),
		huh.NewOption("Yes, make this the default secrets-management profile", initSecretsManagementDefaultYes),
	}, initSecretsManagementDefaultNo)
	document.addEditableSelect(initSecretsManagementFieldAction, "Secrets-management action", "", []huh.Option[string]{
		huh.NewOption("Stage secrets-management settings", initDetailActionEdit),
		huh.NewOption("Back without staging", initDetailActionBack),
	}, initDetailActionEdit)
	editor := initLinearEditor{
		Document: document,
		OnFieldChange: func(model *initLinearEditorModel, index int) {
			if index < 0 || index >= len(model.document) {
				return
			}
			id := model.document[index].ID
			if id == initSecretsManagementFieldTarget {
				initSecretsManagementSyncLinearFields(model, cfg, pendingDeletes, pendingDeleteOrder, true)
				return
			}
			if id == initSecretsManagementFieldBackend {
				initSecretsManagementSyncLinearFields(model, cfg, pendingDeletes, pendingDeleteOrder, false)
			}
		},
		OnEnter: func(model *initLinearEditorModel) (bool, tea.Cmd) {
			if model.focused < 0 || model.focused >= len(model.document) {
				return false, nil
			}
			if model.document[model.focused].ID != initSecretsManagementFieldAction {
				return false, nil
			}
			model.document[model.focused].Error = ""
			switch model.document.selectedValue(initSecretsManagementFieldAction) {
			case initDetailActionBack:
				model.resultAction = initDetailActionBack
				return true, tea.Quit
			case initDetailActionEdit:
				if _, err := initSecretsManagementEditFromDocument(cfg, model.document); err != nil {
					model.document[model.focused].Error = err.Error()
					model.relayout()
					model.ensureFocusedVisible()
					return true, nil
				}
				model.resultAction = initDetailActionEdit
				return true, tea.Quit
			case initSecretsManagementActionDelete:
				if _, err := initSecretsManagementEditFromDocument(cfg, model.document); err != nil {
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
	initSecretsManagementSyncLinearFields(&model, cfg, pendingDeletes, pendingDeleteOrder, true)
	editor.Document = model.document
	return editor
}

func initSecretsManagementTargetOptions(cfg config.File, pendingDeletes map[string]initPendingSecretsManagementDelete, pendingDeleteOrder []string) []huh.Option[string] {
	rows := initSecretsManagementInventoryRows(cfg)
	options := make([]huh.Option[string], 0, len(rows)+len(pendingDeletes))
	commandOptions := make([]huh.Option[string], 0, len(rows))
	for _, row := range rows {
		if row.ID == initBackSelection || !row.Selectable {
			continue
		}
		option := huh.NewOption(row.Title, row.ID)
		if row.Kind == initInventoryRowKindActive && row.ID != initSecretsManagementLegacySelection {
			options = append(options, option)
			continue
		}
		commandOptions = append(commandOptions, option)
	}
	pendingIDs := orderedInitSecretsManagementPendingDeleteIDs(pendingDeletes, pendingDeleteOrder)
	options = append(options, commandOptions...)
	for _, id := range pendingIDs {
		pending := pendingDeletes[id]
		options = append(options, huh.NewOption(initPendingDeleteLabel(initSecretsProfilePendingDeleteTitle(id, pending.Profile)), initSecretsManagementRestoreSelectionPrefix+id))
	}
	return dedupeInitStringOptions(options)
}

func orderedInitSecretsManagementPendingDeleteIDs(pendingDeletes map[string]initPendingSecretsManagementDelete, pendingDeleteOrder []string) []string {
	if len(pendingDeletes) == 0 {
		return nil
	}
	seen := map[string]bool{}
	ordered := make([]string, 0, len(pendingDeletes))
	for _, id := range pendingDeleteOrder {
		if _, ok := pendingDeletes[id]; ok && !seen[id] {
			ordered = append(ordered, id)
			seen[id] = true
		}
	}
	remainder := make([]string, 0, len(pendingDeletes)-len(ordered))
	for id := range pendingDeletes {
		if !seen[id] {
			remainder = append(remainder, id)
		}
	}
	sort.Strings(remainder)
	return append(ordered, remainder...)
}

func appendInitSecretsManagementPendingDeleteOrder(order []string, id string) []string {
	order = removeInitSecretsManagementPendingDeleteOrder(order, id)
	return append(order, id)
}

func removeInitSecretsManagementPendingDeleteOrder(order []string, id string) []string {
	next := order[:0]
	for _, existing := range order {
		if existing != id {
			next = append(next, existing)
		}
	}
	return next
}

type initSecretsManagementSelectionState struct {
	Profile   config.SecretsProfile
	ID        string
	IsDefault bool
	Creating  bool
	Legacy    bool
	Pending   bool
}

func initSecretsManagementSelectionStateForDocument(cfg config.File, document initLinearDocument) (initSecretsManagementSelectionState, error) {
	return initSecretsManagementSelectionStateForSelection(cfg, document.selectedValue(initSecretsManagementFieldTarget))
}

func initSecretsManagementSelectionStateForSelection(cfg config.File, selection string) (initSecretsManagementSelectionState, error) {
	if selection == initSecretsManagementLegacySelection {
		return initSecretsManagementSelectionState{Legacy: true}, nil
	}
	if kind, ok := initSecretsProfileSelectionKind(selection); ok {
		return initSecretsManagementSelectionState{
			Profile:  config.SecretsProfile{Backend: normalizeInitSecretsProfileBackend(config.SecretsProfileBackend{Kind: kind})},
			Creating: true,
		}, nil
	}
	if id, ok := initSecretsManagementRestoreSelectionName(selection); ok {
		return initSecretsManagementSelectionState{ID: id, Pending: true}, nil
	}
	profile, ok := cfg.Secrets.Profiles[selection]
	if !ok {
		return initSecretsManagementSelectionState{}, fmt.Errorf("%w: %s", config.ErrSecretsProfileNotFound, selection)
	}
	return initSecretsManagementSelectionState{
		Profile:   profile,
		ID:        selection,
		IsDefault: strings.TrimSpace(cfg.Secrets.DefaultProfile) == selection,
	}, nil
}

func initSecretsManagementSyncLinearFields(model *initLinearEditorModel, cfg config.File, pendingDeletes map[string]initPendingSecretsManagementDelete, pendingDeleteOrder []string, resetDetails bool) {
	state, err := initSecretsManagementSelectionStateForDocument(cfg, model.document)
	if err != nil {
		return
	}
	initSecretsManagementSetTargetOptions(model, cfg, pendingDeletes, pendingDeleteOrder, model.document.selectedValue(initSecretsManagementFieldTarget))
	profileSectionVisible := !state.Legacy
	profileVisible := profileSectionVisible && !state.Pending
	model.setFieldHidden(initSecretsManagementSectionLegacy, !state.Legacy)
	model.setFieldHidden(initSecretsManagementFieldLegacyBackend, !state.Legacy)
	model.setFieldHidden(initSecretsManagementSectionProfile, !profileSectionVisible)
	model.setFieldHidden(initSecretsManagementFieldLabel, !profileVisible)
	model.setFieldHidden(initSecretsManagementFieldBackend, !profileVisible || state.Creating)
	model.setFieldHidden(initSecretsManagementFieldDefault, !profileVisible)
	initSecretsManagementSetActionOptions(model, !state.Creating && !state.Legacy && !state.Pending)
	if state.Legacy {
		initSecretsManagementSetOnePasswordHidden(model, true, true, true, true)
		return
	}
	if state.Pending {
		model.setFieldHidden(initSecretsManagementFieldAction, false)
		model.setFieldDescription(initSecretsManagementSectionProfile, "This secrets-management profile is staged for deletion. Press r while it is selected to restore it.")
		initSecretsManagementSetOnePasswordHidden(model, true, true, true, true)
		return
	}
	model.setFieldDescription(initSecretsManagementSectionProfile, initSecretsManagementProfileSectionDescription(model.document, state))
	profile := state.Profile
	if strings.TrimSpace(string(profile.Backend.Kind)) == "" {
		profile.Backend = normalizeInitSecretsProfileBackend(config.SecretsProfileBackend{Kind: config.SecretsBackendKind(credstore.BackendKeychain)})
	} else {
		profile.Backend = normalizeInitSecretsProfileBackend(profile.Backend)
	}
	if resetDetails {
		labelSeed := initSecretsProfileEditorLabelSeed(profile, state.ID, profile.Backend.Kind, state.Creating)
		model.setFieldValue(initSecretsManagementFieldLabel, labelSeed.DisplayValue)
		initSecretsManagementSetBackendOptions(model, profile.Backend.Kind, state.Creating)
		model.selectFieldValue(initSecretsManagementFieldBackend, string(profile.Backend.Kind))
		useDefault := initSecretsManagementDefaultNo
		if state.IsDefault {
			useDefault = initSecretsManagementDefaultYes
		}
		model.selectFieldValue(initSecretsManagementFieldDefault, useDefault)
		onePassword := config.SecretsProfileOnePasswordConfig{}
		if profile.Backend.OnePassword != nil {
			onePassword = *profile.Backend.OnePassword
		}
		model.setFieldValue(initSecretsManagementFieldVaultID, onePassword.VaultID)
		model.setFieldValue(initSecretsManagementFieldTimeout, onePassword.Timeout)
		model.setFieldValue(initSecretsManagementFieldItemTitlePrefix, onePassword.ItemTitlePrefix)
		model.setFieldValue(initSecretsManagementFieldItemTag, onePassword.ItemTag)
		model.setFieldValue(initSecretsManagementFieldItemFieldTitle, onePassword.ItemFieldTitle)
		model.setFieldValue(initSecretsManagementFieldConnectHost, onePassword.ConnectHost)
		model.setFieldValue(initSecretsManagementFieldConnectTokenEnv, onePassword.ConnectTokenEnv)
		model.setFieldValue(initSecretsManagementFieldServiceTokenEnv, onePassword.ServiceTokenEnv)
		model.setFieldValue(initSecretsManagementFieldDesktopAccountID, onePassword.DesktopAccountID)
	}
	kind := config.SecretsBackendKind(model.document.selectedValue(initSecretsManagementFieldBackend))
	if state.Creating {
		kind = profile.Backend.Kind
		model.selectFieldValue(initSecretsManagementFieldBackend, string(kind))
	}
	initSecretsManagementSetBackendOptions(model, kind, state.Creating)
	model.setFieldDescription(initSecretsManagementFieldBackend, initSecretsManagementBackendFieldDescription(kind, state.Creating))
	onePassword := config.IsOnePasswordSecretsBackend(kind)
	opConnect := kind == config.SecretsBackendKind(credstore.BackendOPConnect)
	opService := kind == config.SecretsBackendKind(credstore.BackendOP)
	opDesktop := kind == config.SecretsBackendKind(credstore.BackendOPDesktop)
	initSecretsManagementSetOnePasswordHidden(model, !onePassword, !opConnect, !opService, !opDesktop)
	model.setFieldHidden(initSecretsManagementFieldTimeout, !opService && !opDesktop)
	if onePassword && strings.TrimSpace(model.document.fieldValue(initSecretsManagementFieldItemTitlePrefix)) == "" {
		model.setFieldHidden(initSecretsManagementFieldItemTitlePrefix, true)
	}
	if opDesktop && strings.TrimSpace(model.document.fieldValue(initSecretsManagementFieldDesktopAccountID)) == "" {
		model.setFieldHidden(initSecretsManagementFieldDesktopAccountID, true)
	}
	model.setFieldHidden(initSecretsManagementSectionDesktop, !opDesktop || model.document.fieldHidden(initSecretsManagementFieldDesktopAccountID))
}

func initSecretsManagementSetTargetOptions(model *initLinearEditorModel, cfg config.File, pendingDeletes map[string]initPendingSecretsManagementDelete, pendingDeleteOrder []string, selected string) {
	index := model.document.fieldIndexByID(initSecretsManagementFieldTarget)
	if index < 0 {
		return
	}
	options := initLinearOptionsFromHuh(initSecretsManagementTargetOptions(cfg, pendingDeletes, pendingDeleteOrder), selected)
	for optionIndex := range options {
		option := &options[optionIndex]
		if _, configured := cfg.Secrets.Profiles[option.Value]; configured {
			option.Deletable = strings.TrimSpace(cfg.Secrets.DefaultProfile) != option.Value
		}
		if _, restorable := initSecretsManagementRestoreSelectionName(option.Value); restorable {
			option.Restorable = true
		}
	}
	model.document[index].Options = options
}

func initSecretsManagementSetBackendOptions(model *initLinearEditorModel, current config.SecretsBackendKind, locked bool) {
	index := model.document.fieldIndexByID(initSecretsManagementFieldBackend)
	if index < 0 {
		return
	}
	if locked {
		model.document[index].Options = []initLinearOption{{
			Label:    initSecretsManagementBackendOptionLabel(current),
			Value:    string(current),
			Selected: true,
		}}
		return
	}
	selected := model.document.selectedValue(initSecretsManagementFieldBackend)
	if selected == "" {
		selected = string(current)
	}
	model.document[index].Options = initLinearOptionsFromHuh(initSecretsProfileBackendOptions(current), selected)
}

func initSecretsManagementSetActionOptions(model *initLinearEditorModel, canDelete bool) {
	index := model.document.fieldIndexByID(initSecretsManagementFieldAction)
	if index < 0 {
		return
	}
	selected := model.document.selectedValue(initSecretsManagementFieldAction)
	options := []huh.Option[string]{
		huh.NewOption("Stage secrets-management settings", initDetailActionEdit),
		huh.NewOption("Back without staging", initDetailActionBack),
	}
	if selected == initSecretsManagementActionDelete {
		selected = initDetailActionEdit
	}
	model.document[index].Options = initLinearOptionsFromHuh(options, selected)
}

func initSecretsManagementBackendOptionLabel(kind config.SecretsBackendKind) string {
	if backend, ok := initSecretsBackendByKind(kind); ok {
		label := backend.Label
		if !backend.Available {
			label += " (unavailable in this build; existing config)"
		}
		return label
	}
	return string(kind)
}

func initSecretsManagementProfileSectionDescription(document initLinearDocument, state initSecretsManagementSelectionState) string {
	target := initSecretsManagementSelectedOptionLabel(document, initSecretsManagementFieldTarget)
	if state.Pending && target != "" {
		return fmt.Sprintf("Selected target: %s. This profile is staged for deletion. Press r to restore it.", target)
	}
	if state.Creating && target != "" {
		return fmt.Sprintf("Selected target: %s. Fields below configure that new secrets-management profile.", target)
	}
	if state.ID != "" && target != "" {
		return fmt.Sprintf("Selected target: %s. Fields below edit this configured secrets-management profile.", target)
	}
	return "Secrets-management profiles are reusable credential-store definitions that review profiles can choose later."
}

func initSecretsManagementRestoreSelectionName(selection string) (string, bool) {
	if !strings.HasPrefix(selection, initSecretsManagementRestoreSelectionPrefix) {
		return "", false
	}
	return strings.TrimPrefix(selection, initSecretsManagementRestoreSelectionPrefix), true
}

func initSecretsProfilePendingDeleteTitle(id string, profile config.SecretsProfile) string {
	return initSecretsProfileInventoryTitle(config.EffectiveSecretsProfile{
		ID:      id,
		Label:   profile.Label,
		Backend: string(profile.Backend.Kind),
		Source:  config.EffectiveSecretsProfileSourceConfigured,
	})
}

func initSecretsManagementBackendFieldDescription(kind config.SecretsBackendKind, locked bool) string {
	description := strings.TrimSpace(initSecretsBackendDescription(kind))
	if locked {
		return strings.TrimSpace(description + " This backend is fixed by the selected create-new target; choose a different target above to create a different backend type.")
	}
	return strings.TrimSpace(description + " Use Up/Down here to change the backend for this configured secrets-management profile.")
}

func initSecretsManagementSelectedOptionLabel(document initLinearDocument, id initLinearFieldID) string {
	index := document.fieldIndexByID(id)
	if index < 0 {
		return ""
	}
	for _, option := range document[index].Options {
		if option.Selected {
			return option.Label
		}
	}
	return ""
}

func initSecretsManagementSetOnePasswordHidden(model *initLinearEditorModel, hideOnePassword bool, hideConnect bool, hideService bool, hideDesktop bool) {
	model.setFieldHidden(initSecretsManagementSectionOnePassword, hideOnePassword)
	model.setFieldHidden(initSecretsManagementFieldVaultID, hideOnePassword)
	model.setFieldHidden(initSecretsManagementFieldItemTitlePrefix, hideOnePassword)
	model.setFieldHidden(initSecretsManagementFieldItemTag, hideOnePassword)
	model.setFieldHidden(initSecretsManagementFieldItemFieldTitle, hideOnePassword)
	model.setFieldHidden(initSecretsManagementSectionConnect, hideOnePassword || hideConnect)
	model.setFieldHidden(initSecretsManagementFieldConnectHost, hideOnePassword || hideConnect)
	model.setFieldHidden(initSecretsManagementFieldConnectTokenEnv, hideOnePassword || hideConnect)
	model.setFieldHidden(initSecretsManagementSectionServiceAccount, hideOnePassword || hideService)
	model.setFieldHidden(initSecretsManagementFieldServiceTokenEnv, hideOnePassword || hideService)
	model.setFieldHidden(initSecretsManagementSectionDesktop, hideOnePassword || hideDesktop)
	model.setFieldHidden(initSecretsManagementFieldDesktopAccountID, hideOnePassword || hideDesktop)
	model.setFieldHidden(initSecretsManagementFieldTimeout, hideOnePassword)
}

func initSecretsManagementEditFromDocument(cfg config.File, document initLinearDocument) (initKeyringBackendEdit, error) {
	state, err := initSecretsManagementSelectionStateForDocument(cfg, document)
	if err != nil {
		return initKeyringBackendEdit{}, err
	}
	working := cloneInitConfigFile(cfg)
	if state.Legacy {
		working.Keyring.Backend = strings.TrimSpace(document.selectedValue(initSecretsManagementFieldLegacyBackend))
		return initKeyringBackendEdit{Apply: true, HasConfigEdit: true, Config: config.Normalize(working)}, nil
	}
	if state.Pending {
		return initKeyringBackendEdit{Apply: true, HasConfigEdit: true, Config: config.Normalize(working)}, nil
	}
	if document.selectedValue(initSecretsManagementFieldAction) == initSecretsManagementActionDelete {
		if state.Creating || state.ID == "" {
			return initKeyringBackendEdit{}, fmt.Errorf("only configured secrets-management profiles can be deleted")
		}
		nextCfg, _, err := configedit.RemoveSecretsProfile(working, state.ID)
		if err != nil {
			return initKeyringBackendEdit{}, err
		}
		return initKeyringBackendEdit{Apply: true, HasConfigEdit: true, Config: nextCfg}, nil
	}
	edit, err := initSecretsManagementProfileEditFromDocument(state, document)
	if err != nil {
		return initKeyringBackendEdit{}, err
	}
	if state.Creating {
		id := initSecretsProfileIDFromLabel(edit.StoredLabel, edit.Backend.Kind, working.Secrets.Profiles)
		patch := configedit.SecretsProfilePatch{Backend: &edit.Backend}
		if edit.StoredLabel != "" {
			label := edit.StoredLabel
			patch.Label = &label
		}
		nextCfg, _, _, err := configedit.SetSecretsProfile(working, id, patch)
		if err != nil {
			return initKeyringBackendEdit{}, err
		}
		if edit.UseDefault {
			nextCfg, _, err = configedit.SetDefaultSecretsProfile(nextCfg, id)
		}
		if err != nil {
			return initKeyringBackendEdit{}, err
		}
		return initKeyringBackendEdit{Apply: true, HasConfigEdit: true, Config: nextCfg}, nil
	}
	patch := configedit.SecretsProfilePatch{Backend: &edit.Backend}
	if edit.StoredLabel != "" {
		label := edit.StoredLabel
		patch.Label = &label
	} else {
		patch.ClearLabel = true
	}
	nextCfg, _, _, err := configedit.SetSecretsProfile(working, state.ID, patch)
	if err != nil {
		return initKeyringBackendEdit{}, err
	}
	if edit.UseDefault {
		nextCfg, _, err = configedit.SetDefaultSecretsProfile(nextCfg, state.ID)
	} else if strings.TrimSpace(working.Secrets.DefaultProfile) == state.ID {
		nextCfg, _, err = configedit.UnsetDefaultSecretsProfile(nextCfg)
	}
	if err != nil {
		return initKeyringBackendEdit{}, err
	}
	return initKeyringBackendEdit{Apply: true, HasConfigEdit: true, Config: nextCfg}, nil
}

func initSecretsManagementProfileEditFromDocument(state initSecretsManagementSelectionState, document initLinearDocument) (initSecretsProfileEditorResult, error) {
	profile := state.Profile
	if strings.TrimSpace(string(profile.Backend.Kind)) == "" {
		profile.Backend = normalizeInitSecretsProfileBackend(config.SecretsProfileBackend{Kind: config.SecretsBackendKind(credstore.BackendKeychain)})
	} else {
		profile.Backend = normalizeInitSecretsProfileBackend(profile.Backend)
	}
	labelSeed := initSecretsProfileEditorLabelSeed(profile, state.ID, profile.Backend.Kind, state.Creating)
	labelInput := document.fieldValue(initSecretsManagementFieldLabel)
	if err := validateOptionalDisplayName(labelInput); err != nil {
		return initSecretsProfileEditorResult{}, err
	}
	kindValue := document.selectedValue(initSecretsManagementFieldBackend)
	kind := config.SecretsBackendKind(kindValue)
	vaultValue := document.fieldValue(initSecretsManagementFieldVaultID)
	if config.IsOnePasswordSecretsBackend(kind) {
		if err := validateInitSecretsRequiredSingleLine(vaultValue, true, "1Password vault name or id"); err != nil {
			return initSecretsProfileEditorResult{}, err
		}
	}
	if kind == config.SecretsBackendKind(credstore.BackendOPConnect) {
		if err := validateInitSecretsRequiredSingleLine(document.fieldValue(initSecretsManagementFieldConnectHost), true, "1Password Connect host"); err != nil {
			return initSecretsProfileEditorResult{}, err
		}
	}
	for _, field := range []struct {
		id       initLinearFieldID
		validate func(string) error
	}{
		{initSecretsManagementFieldTimeout, validateOptionalDuration},
		{initSecretsManagementFieldItemTitlePrefix, validateOptionalDisplayName},
		{initSecretsManagementFieldItemTag, validateOptionalDisplayName},
		{initSecretsManagementFieldItemFieldTitle, validateOptionalDisplayName},
		{initSecretsManagementFieldConnectTokenEnv, validateOptionalDisplayName},
		{initSecretsManagementFieldServiceTokenEnv, validateOptionalDisplayName},
		{initSecretsManagementFieldDesktopAccountID, validateOptionalDisplayName},
	} {
		if err := field.validate(document.fieldValue(field.id)); err != nil {
			return initSecretsProfileEditorResult{}, err
		}
	}
	backend := initSecretsProfileBackendFromInputs(
		kindValue,
		document.fieldValue(initSecretsManagementFieldTimeout),
		vaultValue,
		document.fieldValue(initSecretsManagementFieldItemTitlePrefix),
		document.fieldValue(initSecretsManagementFieldItemTag),
		document.fieldValue(initSecretsManagementFieldItemFieldTitle),
		document.fieldValue(initSecretsManagementFieldConnectHost),
		document.fieldValue(initSecretsManagementFieldConnectTokenEnv),
		document.fieldValue(initSecretsManagementFieldServiceTokenEnv),
		document.fieldValue(initSecretsManagementFieldDesktopAccountID),
	)
	return initSecretsProfileEditorResult{
		Apply:       true,
		Label:       strings.TrimSpace(labelInput),
		StoredLabel: normalizeInitSecretsProfileStoredLabel(labelInput, labelSeed.FallbackValue, labelSeed.StoredLabel, state.Creating),
		Backend:     backend,
		UseDefault:  document.selectedValue(initSecretsManagementFieldDefault) == initSecretsManagementDefaultYes,
	}, nil
}

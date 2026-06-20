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
	initSecretsManagementFieldTarget            initLinearFieldID = "secrets_management_target"
	initSecretsManagementFieldLabel             initLinearFieldID = "secrets_management_label"
	initSecretsManagementFieldBackend           initLinearFieldID = "secrets_management_backend"
	initSecretsManagementFieldVaultID           initLinearFieldID = "secrets_management_1password_vault_id"
	initSecretsManagementFieldTimeout           initLinearFieldID = "secrets_management_1password_timeout"
	initSecretsManagementFieldDesktopSelection  initLinearFieldID = "secrets_management_1password_desktop_selection"
	initSecretsManagementFieldDesktopAccountURL initLinearFieldID = "secrets_management_1password_desktop_account_url"
	initSecretsManagementFieldConnectHost       initLinearFieldID = "secrets_management_1password_connect_host"
	initSecretsManagementFieldConnectTokenEnv   initLinearFieldID = "secrets_management_1password_connect_token_env"
	initSecretsManagementFieldServiceTokenEnv   initLinearFieldID = "secrets_management_1password_service_token_env"
	initSecretsManagementFieldDesktopAccountID  initLinearFieldID = "secrets_management_1password_desktop_account_id"
	initSecretsManagementFieldAction            initLinearFieldID = "secrets_management_action"
	initSecretsManagementSectionProfile         initLinearFieldID = "secrets_management_section_profile"
	initSecretsManagementSectionOnePassword     initLinearFieldID = "secrets_management_section_1password"
	initSecretsManagementSectionConnect         initLinearFieldID = "secrets_management_section_connect"
	initSecretsManagementSectionServiceAccount  initLinearFieldID = "secrets_management_section_service_account"
	initSecretsManagementSectionDesktop         initLinearFieldID = "secrets_management_section_desktop"
	initSecretsManagementActionDelete           string            = "delete"
)

const initSecretsManagementRestoreSelectionPrefix = "__restore_secrets_management__:"

type initPendingSecretsManagementDelete struct {
	ID      string
	Profile config.SecretsProfile
}

func (p huhInitKeyringBackendPrompter) editKeyringBackendLinear(prompt initKeyringBackendPrompt) (initKeyringBackendEdit, error) {
	working := config.Normalize(cloneInitConfigFile(prompt.Config))
	p.writeSecretsStorageDiscoveryNotice()
	desktopDiscovery := p.discoverOnePasswordDesktop()
	pendingDeletes := map[string]initPendingSecretsManagementDelete{}
	pendingDeleteOrder := []string{}
	for {
		editor := initSecretsManagementLinearEditorWithPendingOrderAndDiscovery(working, pendingDeletes, pendingDeleteOrder, desktopDiscovery)
		model, err := p.runSecretsManagementEditor(editor)
		if err != nil {
			return initKeyringBackendEdit{}, err
		}
		switch model.resultAction {
		case initDetailActionEdit:
			return initSecretsManagementEditFromDocumentWithDiscovery(working, model.document, desktopDiscovery)
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

func (p huhInitKeyringBackendPrompter) writeSecretsStorageDiscoveryNotice() {
	if p.stderr == nil {
		return
	}
	_, _ = fmt.Fprintln(p.stderr, "Checking available secrets storage backends.")
	_, _ = fmt.Fprintln(p.stderr, "You may see permission prompts from 1Password, macOS, or your terminal app.")
	_, _ = fmt.Fprintln(p.stderr)
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
	return initSecretsManagementLinearEditorWithPendingOrderAndDiscovery(cfg, pendingDeletes, pendingDeleteOrder, initOnePasswordDesktopDiscovery{})
}

func initSecretsManagementLinearEditorWithPendingOrderAndDiscovery(cfg config.File, pendingDeletes map[string]initPendingSecretsManagementDelete, pendingDeleteOrder []string, desktopDiscovery initOnePasswordDesktopDiscovery) initLinearEditor {
	targetOptions := initSecretsManagementTargetOptions(cfg, pendingDeletes, pendingDeleteOrder)
	selectedTarget := normalizeInitStringSelectionValue("", targetOptions)
	var document initLinearDocument
	document.addSection("Secrets storage", initSecretsManagementInventoryDescription())
	document.addEditableSelect(initSecretsManagementFieldTarget, "Credential store", "", targetOptions, selectedTarget)
	document.addSectionField(initSecretsManagementSectionProfile, "Credential store", "Configured credential stores are reusable destinations for secrets.")
	document.addEditableInput(
		initSecretsManagementFieldLabel,
		"Credential store name",
		"Choose a human-friendly name for this credential store.",
		"",
		validateOptionalDisplayName,
	)
	document.addEditableSelect(initSecretsManagementFieldBackend, "Credential store backend", "", initSecretsProfileBackendOptions(config.SecretsBackendKind(credstore.BackendFile)), string(credstore.BackendFile))
	document.addSectionField(initSecretsManagementSectionOnePassword, "1Password details", "These are non-secret 1Password settings. Tokens are referenced by environment variable name, not collected here.")
	document.addSectionField(initSecretsManagementSectionDesktop, "1Password desktop", "Desktop app account and vault routing.")
	document.addEditableSelect(initSecretsManagementFieldDesktopSelection, "1Password account and vault", "Discovered from the local 1Password desktop app. Choose manual entry if this list is incomplete.", desktopDiscovery.Options(), initOnePasswordManualSelection)
	document.addEditableInput(initSecretsManagementFieldDesktopAccountURL, "1Password account URL", "Account sign-in address such as signalft.1password.com. Required only when desktop discovery is unavailable or manual entry is selected.", "", validateOptionalDisplayName)
	document.addEditableInput(initSecretsManagementFieldDesktopAccountID, "1Password account id (advanced)", "Advanced. Optional account id when you need to pin this profile to one signed-in 1Password desktop account.", "", validateOptionalDisplayName)
	document.addEditableInput(initSecretsManagementFieldVaultID, "1Password vault name or id", "Required for every 1Password-backed credential store. Enter a vault name such as Personal, Employee, or My Vault, or enter a stable vault ID.", "", nil)
	document.addEditableInput(initSecretsManagementFieldTimeout, "1Password request timeout", "How long cr waits for one 1Password backend request before failing. Leave the default unless your 1Password integration is unusually slow.", "", validateOptionalDuration)
	document.addSectionField(initSecretsManagementSectionConnect, "1Password Connect", "Required only for 1Password Connect profiles.")
	document.addEditableInput(initSecretsManagementFieldConnectHost, "1Password Connect host", "Required only for 1Password Connect profiles.", "", nil)
	document.addEditableInput(initSecretsManagementFieldConnectTokenEnv, "1Password Connect token env var", "Environment variable that holds the 1Password Connect token.", "", validateOptionalDisplayName)
	document.addSectionField(initSecretsManagementSectionServiceAccount, "1Password service account", "Required only for 1Password service-account profiles.")
	document.addEditableInput(initSecretsManagementFieldServiceTokenEnv, "1Password service token env var", "Environment variable that holds the 1Password service-account token.", "", validateOptionalDisplayName)
	document.addEditableSelect(initSecretsManagementFieldAction, "Secrets-storage action", "", []huh.Option[string]{
		huh.NewOption("Stage secrets-storage settings", initDetailActionEdit),
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
				initSecretsManagementSyncLinearFields(model, cfg, pendingDeletes, pendingDeleteOrder, desktopDiscovery, true)
				return
			}
			if id == initSecretsManagementFieldBackend || id == initSecretsManagementFieldDesktopSelection {
				initSecretsManagementSyncLinearFields(model, cfg, pendingDeletes, pendingDeleteOrder, desktopDiscovery, false)
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
				if _, err := initSecretsManagementEditFromDocumentWithDiscovery(cfg, model.document, desktopDiscovery); err != nil {
					model.document[model.focused].Error = err.Error()
					model.relayout()
					model.ensureFocusedVisible()
					return true, nil
				}
				model.resultAction = initDetailActionEdit
				return true, tea.Quit
			case initSecretsManagementActionDelete:
				if _, err := initSecretsManagementEditFromDocumentWithDiscovery(cfg, model.document, desktopDiscovery); err != nil {
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
	initSecretsManagementSyncLinearFields(&model, cfg, pendingDeletes, pendingDeleteOrder, desktopDiscovery, true)
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
		if row.Kind == initInventoryRowKindActive {
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
	Profile  config.SecretsProfile
	ID       string
	Creating bool
	BuiltIn  bool
	Pending  bool
}

func initSecretsManagementSelectionStateForDocument(cfg config.File, document initLinearDocument) (initSecretsManagementSelectionState, error) {
	return initSecretsManagementSelectionStateForSelection(cfg, document.selectedValue(initSecretsManagementFieldTarget))
}

func initSecretsManagementSelectionStateForSelection(cfg config.File, selection string) (initSecretsManagementSelectionState, error) {
	cfg = config.Normalize(cfg)
	if selection == config.LocalOSCredentialStoreID {
		return initSecretsManagementSelectionState{ID: config.LocalOSCredentialStoreID, BuiltIn: true}, nil
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
	profile, ok := cfg.Secrets.Stores[selection]
	if !ok {
		return initSecretsManagementSelectionState{}, fmt.Errorf("%w: %s", config.ErrSecretsProfileNotFound, selection)
	}
	return initSecretsManagementSelectionState{
		Profile: profile,
		ID:      selection,
	}, nil
}

func initSecretsManagementSyncLinearFields(model *initLinearEditorModel, cfg config.File, pendingDeletes map[string]initPendingSecretsManagementDelete, pendingDeleteOrder []string, desktopDiscovery initOnePasswordDesktopDiscovery, resetDetails bool) {
	state, err := initSecretsManagementSelectionStateForDocument(cfg, model.document)
	if err != nil {
		return
	}
	initSecretsManagementSetTargetOptions(model, cfg, pendingDeletes, pendingDeleteOrder, model.document.selectedValue(initSecretsManagementFieldTarget))
	profileVisible := !state.Pending && !state.BuiltIn
	allowEdit := profileVisible || state.Pending
	model.setFieldHidden(initSecretsManagementSectionProfile, false)
	model.setFieldHidden(initSecretsManagementFieldLabel, !profileVisible)
	model.setFieldHidden(initSecretsManagementFieldBackend, !profileVisible || state.Creating)
	initSecretsManagementSetActionOptions(model, allowEdit)
	if state.BuiltIn {
		model.setFieldDescription(initSecretsManagementSectionProfile, initSecretsManagementProfileSectionDescription(model.document, state))
		initSecretsManagementSetOnePasswordHidden(model, true, true, true, true)
		model.selectFieldValue(initSecretsManagementFieldAction, initDetailActionBack)
		return
	}
	if state.Pending {
		model.setFieldHidden(initSecretsManagementFieldAction, false)
		model.setFieldDescription(initSecretsManagementSectionProfile, "This credential store is staged for deletion. Press r while it is selected to restore it.")
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
		onePassword := config.SecretsProfileOnePasswordConfig{}
		if profile.Backend.OnePassword != nil {
			onePassword = *profile.Backend.OnePassword
		}
		model.setFieldValue(initSecretsManagementFieldVaultID, onePassword.VaultID)
		model.setFieldValue(initSecretsManagementFieldTimeout, onePassword.Timeout)
		model.setFieldValue(initSecretsManagementFieldDesktopAccountURL, onePassword.AccountURL)
		accountID := onePassword.AccountID
		if accountID == "" {
			accountID = onePassword.DesktopAccountID
		}
		model.setFieldValue(initSecretsManagementFieldDesktopAccountID, accountID)
		desktopSelection := desktopDiscovery.SelectionFor(accountID, onePassword.AccountURL, onePassword.VaultID, onePassword.VaultName)
		if desktopSelection == initOnePasswordManualSelection && state.Creating && accountID == "" && onePassword.AccountURL == "" && onePassword.VaultID == "" && onePassword.VaultName == "" {
			if options := desktopDiscovery.Options(); len(options) > 0 {
				desktopSelection = options[0].Value
			}
		}
		initSecretsManagementSetDesktopDiscoveryOptions(model, desktopDiscovery, desktopSelection)
		model.setFieldValue(initSecretsManagementFieldConnectHost, onePassword.ConnectHost)
		model.setFieldValue(initSecretsManagementFieldConnectTokenEnv, onePassword.ConnectTokenEnv)
		model.setFieldValue(initSecretsManagementFieldServiceTokenEnv, onePassword.ServiceTokenEnv)
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
	if opDesktop {
		initSecretsManagementSetDesktopDiscoveryOptions(model, desktopDiscovery, model.document.selectedValue(initSecretsManagementFieldDesktopSelection))
	}
	model.setFieldHidden(initSecretsManagementFieldTimeout, !opService && !opDesktop)
	discoveredDesktop := opDesktop && desktopDiscovery.HasVaultChoices()
	manualDesktop := opDesktop && (!discoveredDesktop || model.document.selectedValue(initSecretsManagementFieldDesktopSelection) == initOnePasswordManualSelection)
	model.setFieldHidden(initSecretsManagementFieldDesktopSelection, !discoveredDesktop)
	model.setFieldHidden(initSecretsManagementFieldDesktopAccountURL, !manualDesktop)
	model.setFieldHidden(initSecretsManagementFieldDesktopAccountID, !manualDesktop)
	model.setFieldHidden(initSecretsManagementFieldVaultID, !onePassword || (discoveredDesktop && !manualDesktop))
	model.setFieldHidden(initSecretsManagementSectionDesktop, !opDesktop)
}

func initSecretsManagementSetTargetOptions(model *initLinearEditorModel, cfg config.File, pendingDeletes map[string]initPendingSecretsManagementDelete, pendingDeleteOrder []string, selected string) {
	index := model.document.fieldIndexByID(initSecretsManagementFieldTarget)
	if index < 0 {
		return
	}
	cfg = config.Normalize(cfg)
	options := initLinearOptionsFromHuh(initSecretsManagementTargetOptions(cfg, pendingDeletes, pendingDeleteOrder), selected)
	for optionIndex := range options {
		option := &options[optionIndex]
		if _, configured := cfg.Secrets.Stores[option.Value]; configured {
			option.Deletable = option.Value != config.LocalOSCredentialStoreID
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

func initSecretsManagementSetDesktopDiscoveryOptions(model *initLinearEditorModel, discovery initOnePasswordDesktopDiscovery, selected string) {
	index := model.document.fieldIndexByID(initSecretsManagementFieldDesktopSelection)
	if index < 0 {
		return
	}
	if strings.TrimSpace(selected) == "" {
		selected = initOnePasswordManualSelection
	}
	model.document[index].Options = discovery.LinearOptions(selected)
	if len(model.document[index].Options) == 0 {
		model.document[index].Options = []initLinearOption{{
			Label:    "Enter account and vault manually",
			Value:    initOnePasswordManualSelection,
			Selected: true,
		}}
	}
}

func initSecretsManagementSetActionOptions(model *initLinearEditorModel, allowEdit bool) {
	index := model.document.fieldIndexByID(initSecretsManagementFieldAction)
	if index < 0 {
		return
	}
	selected := model.document.selectedValue(initSecretsManagementFieldAction)
	options := []huh.Option[string]{
		huh.NewOption("Back without staging", initDetailActionBack),
	}
	if allowEdit {
		options = append([]huh.Option[string]{
			huh.NewOption("Stage secrets-storage settings", initDetailActionEdit),
		}, options...)
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
		return fmt.Sprintf("Selected target: %s. This credential store is staged for deletion. Press r to restore it.", target)
	}
	if state.BuiltIn && target != "" {
		description := strings.TrimSpace(initBuiltInOSCredentialStoreDescription())
		if description != "" {
			return fmt.Sprintf("Selected target: %s, %s. This built-in credential store is always available and cannot be deleted.", target, description)
		}
		return fmt.Sprintf("Selected target: %s. This built-in credential store is always available and cannot be deleted.", target)
	}
	if state.Creating && target != "" {
		return fmt.Sprintf("Selected target: %s. Fields below configure that new credential store.", target)
	}
	if state.ID != "" && target != "" {
		return fmt.Sprintf("Selected target: %s. Fields below edit this configured credential store.", target)
	}
	return "Configured credential stores are reusable destinations for secrets."
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
		Label:   profile.DisplayName,
		Backend: string(profile.Backend.Kind),
		Source:  config.EffectiveSecretsProfileSourceConfigured,
	})
}

func initSecretsManagementBackendFieldDescription(kind config.SecretsBackendKind, locked bool) string {
	description := strings.TrimSpace(initSecretsBackendDescription(kind))
	if locked {
		return strings.TrimSpace(description + " This backend is fixed by the selected create-new target; choose a different target above to create a different backend type.")
	}
	return strings.TrimSpace(description + " Use Up/Down here to change the backend for this configured credential store.")
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
	model.setFieldHidden(initSecretsManagementSectionConnect, hideOnePassword || hideConnect)
	model.setFieldHidden(initSecretsManagementFieldConnectHost, hideOnePassword || hideConnect)
	model.setFieldHidden(initSecretsManagementFieldConnectTokenEnv, hideOnePassword || hideConnect)
	model.setFieldHidden(initSecretsManagementSectionServiceAccount, hideOnePassword || hideService)
	model.setFieldHidden(initSecretsManagementFieldServiceTokenEnv, hideOnePassword || hideService)
	model.setFieldHidden(initSecretsManagementSectionDesktop, hideOnePassword || hideDesktop)
	model.setFieldHidden(initSecretsManagementFieldDesktopSelection, hideOnePassword || hideDesktop)
	model.setFieldHidden(initSecretsManagementFieldDesktopAccountURL, hideOnePassword || hideDesktop)
	model.setFieldHidden(initSecretsManagementFieldDesktopAccountID, hideOnePassword || hideDesktop)
	model.setFieldHidden(initSecretsManagementFieldTimeout, hideOnePassword)
}

func initSecretsManagementEditFromDocument(cfg config.File, document initLinearDocument) (initKeyringBackendEdit, error) {
	return initSecretsManagementEditFromDocumentWithDiscovery(cfg, document, initOnePasswordDesktopDiscovery{})
}

func initSecretsManagementEditFromDocumentWithDiscovery(cfg config.File, document initLinearDocument, desktopDiscovery initOnePasswordDesktopDiscovery) (initKeyringBackendEdit, error) {
	state, err := initSecretsManagementSelectionStateForDocument(cfg, document)
	if err != nil {
		return initKeyringBackendEdit{}, err
	}
	working := config.Normalize(cloneInitConfigFile(cfg))
	if state.BuiltIn {
		return initKeyringBackendEdit{}, nil
	}
	if state.Pending {
		return initKeyringBackendEdit{Apply: true, HasConfigEdit: true, Config: config.Normalize(working)}, nil
	}
	if document.selectedValue(initSecretsManagementFieldAction) == initSecretsManagementActionDelete {
		if state.Creating || state.ID == "" {
			return initKeyringBackendEdit{}, fmt.Errorf("only configured credential stores can be deleted")
		}
		nextCfg, _, err := configedit.RemoveSecretsProfile(working, state.ID)
		if err != nil {
			return initKeyringBackendEdit{}, err
		}
		return initKeyringBackendEdit{Apply: true, HasConfigEdit: true, Config: nextCfg}, nil
	}
	edit, err := initSecretsManagementProfileEditFromDocument(state, document, desktopDiscovery)
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
	return initKeyringBackendEdit{Apply: true, HasConfigEdit: true, Config: nextCfg}, nil
}

func initSecretsManagementProfileEditFromDocument(state initSecretsManagementSelectionState, document initLinearDocument, desktopDiscovery initOnePasswordDesktopDiscovery) (initSecretsProfileEditorResult, error) {
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
	vaultName := ""
	accountID := document.fieldValue(initSecretsManagementFieldDesktopAccountID)
	accountURL := document.fieldValue(initSecretsManagementFieldDesktopAccountURL)
	if kind == config.SecretsBackendKind(credstore.BackendOPDesktop) && desktopDiscovery.HasVaultChoices() && document.selectedValue(initSecretsManagementFieldDesktopSelection) != initOnePasswordManualSelection {
		if selection, ok := desktopDiscovery.Selection(document.selectedValue(initSecretsManagementFieldDesktopSelection)); ok {
			accountID = selection.AccountID
			accountURL = selection.AccountURL
			vaultValue = selection.VaultID
			vaultName = selection.VaultName
		}
	}
	if config.IsOnePasswordSecretsBackend(kind) {
		requiresVault := true
		if kind == config.SecretsBackendKind(credstore.BackendOPDesktop) && desktopDiscovery.HasVaultChoices() && document.selectedValue(initSecretsManagementFieldDesktopSelection) != initOnePasswordManualSelection {
			requiresVault = false
		}
		if err := validateInitSecretsRequiredSingleLine(vaultValue, requiresVault, "1Password vault name or id"); err != nil {
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
		{initSecretsManagementFieldConnectTokenEnv, validateOptionalDisplayName},
		{initSecretsManagementFieldServiceTokenEnv, validateOptionalDisplayName},
		{initSecretsManagementFieldDesktopAccountURL, validateOptionalDisplayName},
		{initSecretsManagementFieldDesktopAccountID, validateOptionalDisplayName},
	} {
		if err := field.validate(document.fieldValue(field.id)); err != nil {
			return initSecretsProfileEditorResult{}, err
		}
	}
	backend := initSecretsProfileBackendFromInputs(initSecretsProfileBackendInput{
		KindValue:       kindValue,
		Timeout:         document.fieldValue(initSecretsManagementFieldTimeout),
		AccountID:       accountID,
		AccountURL:      accountURL,
		VaultID:         vaultValue,
		VaultName:       vaultName,
		ConnectHost:     document.fieldValue(initSecretsManagementFieldConnectHost),
		ConnectTokenEnv: document.fieldValue(initSecretsManagementFieldConnectTokenEnv),
		ServiceTokenEnv: document.fieldValue(initSecretsManagementFieldServiceTokenEnv),
	})
	return initSecretsProfileEditorResult{
		Apply:       true,
		Label:       strings.TrimSpace(labelInput),
		StoredLabel: normalizeInitSecretsProfileStoredLabel(labelInput, labelSeed.FallbackValue, labelSeed.StoredLabel, state.Creating),
		Backend:     backend,
	}, nil
}

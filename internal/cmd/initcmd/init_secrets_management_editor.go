package initcmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/open-cli-collective/cli-common/credstore"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/configedit"
)

const (
	initSecretsManagementFieldTarget            initLinearFieldID = "secrets_management_target"
	initSecretsManagementFieldLabel             initLinearFieldID = "secrets_management_label"
	initSecretsManagementFieldBackend           initLinearFieldID = "secrets_management_backend"
	initSecretsManagementFieldVaultID           initLinearFieldID = "secrets_management_1password_vault_id"
	initSecretsManagementFieldTimeout           initLinearFieldID = "secrets_management_1password_timeout"
	initSecretsManagementFieldDesktopAccount    initLinearFieldID = "secrets_management_1password_desktop_account"
	initSecretsManagementFieldDesktopVault      initLinearFieldID = "secrets_management_1password_desktop_vault"
	initSecretsManagementFieldDesktopAccountURL initLinearFieldID = "secrets_management_1password_desktop_account_url"
	initSecretsManagementFieldConnectHost       initLinearFieldID = "secrets_management_1password_connect_host"
	initSecretsManagementFieldConnectTokenEnv   initLinearFieldID = "secrets_management_1password_connect_token_env"
	initSecretsManagementFieldServiceTokenEnv   initLinearFieldID = "secrets_management_1password_service_token_env"
	initSecretsManagementFieldDesktopAccountID  initLinearFieldID = "secrets_management_1password_desktop_account_id"
	initSecretsManagementFieldAction            initLinearFieldID = "secrets_management_action"
	initSecretsManagementSectionBuiltIn         initLinearFieldID = "secrets_management_section_builtin"
	initSecretsManagementSectionProfile         initLinearFieldID = "secrets_management_section_profile"
	initSecretsManagementSectionConnect         initLinearFieldID = "secrets_management_section_connect"
	initSecretsManagementSectionServiceAccount  initLinearFieldID = "secrets_management_section_service_account"
)

type initPendingSecretsManagementDelete struct {
	ID      string
	Profile config.SecretsStore
}

func (p huhInitKeyringBackendPrompter) editKeyringBackendLinear(prompt initKeyringBackendPrompt) (initKeyringBackendEdit, error) {
	working := config.Normalize(cloneInitConfigFile(prompt.Config))
	discoveryMode := p.resolvedDiscoveryMode()
	p.writeSecretsStorageDiscoveryNotice(discoveryMode)
	desktopDiscovery := p.discoverOnePasswordDesktopForMode(discoveryMode)
	p.writeSecretsStorageDiscoveryResults(discoveryMode, desktopDiscovery)
	pendingDeletes := map[string]initPendingSecretsManagementDelete{}
	pendingDeleteOrder := []string{}
	for {
		editor := initSecretsManagementLinearEditorWithPendingOrderAndDiscovery(working, pendingDeletes, pendingDeleteOrder, desktopDiscovery)
		model, err := runInitEditor(editor, p.stdin, p.stderr, p.editorRunner, "secrets-management")
		if err != nil {
			return initKeyringBackendEdit{}, err
		}
		switch model.resultAction {
		case initDetailActionEdit:
			return initSecretsManagementEditFromDocumentWithDiscovery(working, model.document, desktopDiscovery)
		case initLinearResultActionDelete:
			edit, err := initSecretsManagementDeleteEditFromDocument(working, model.document)
			if err != nil {
				return initKeyringBackendEdit{}, err
			}
			return edit, nil
		case initLinearResultActionRestore:
			selection := model.document.selectedValue(initSecretsManagementFieldTarget)
			id, ok := initLinearRestoreSelectionName("secrets_management", selection)
			if !ok {
				continue
			}
			pending, ok := pendingDeletes[id]
			if !ok {
				continue
			}
			patch := configedit.SecretsStorePatch{Backend: &pending.Profile.Backend}
			if strings.TrimSpace(pending.Profile.DisplayName) != "" {
				label := pending.Profile.DisplayName
				patch.Label = &label
			}
			nextCfg, _, _, err := configedit.SetSecretsStore(working, id, patch)
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

func (p huhInitKeyringBackendPrompter) writeSecretsStorageDiscoveryNotice(mode initSecretsBackendDiscoveryMode) {
	if p.stderr == nil {
		return
	}
	switch mode {
	case initSecretsBackendDiscoveryModeSafe:
		_, _ = fmt.Fprintln(p.stderr, "Checking available secrets storage backends.")
		_, _ = fmt.Fprintln(p.stderr, "Only passive discovery is enabled; inventory probes are skipped.")
	case initSecretsBackendDiscoveryModeOff:
		_, _ = fmt.Fprintln(p.stderr, "Secrets backend discovery is disabled.")
	case initSecretsBackendDiscoveryModeFull:
		_, _ = fmt.Fprintln(p.stderr, "Checking available secrets storage backends.")
		_, _ = fmt.Fprintln(p.stderr, "You may see permission prompts from 1Password, macOS, or your terminal app.")
	default:
		_, _ = fmt.Fprintln(p.stderr, "Checking available secrets storage backends.")
		_, _ = fmt.Fprintln(p.stderr, "You may see permission prompts from 1Password, macOS, or your terminal app.")
	}
	_, _ = fmt.Fprintln(p.stderr)
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
	document.addSectionField(initSecretsManagementSectionBuiltIn, "Built in", initSecretsManagementBuiltInSectionDescription())
	document.addEditableSelect(initSecretsManagementFieldTarget, "Actions", "Configure a new credential store or edit a configured store.", targetOptions, selectedTarget)
	document.addSectionField(initSecretsManagementSectionProfile, "Credential store details", "Configured credential stores are reusable destinations for secrets.")
	document.addEditableSelect(initSecretsManagementFieldBackend, "Credential store backend", "", initSecretsStoreBackendOptions(config.SecretsBackendKind(credstore.BackendFile)), string(credstore.BackendFile))
	document.addEditableSelect(initSecretsManagementFieldDesktopAccount, "1Password account", "Discovered from the local 1Password desktop app.", desktopDiscovery.AccountOptions(), initOnePasswordManualSelection)
	document.addEditableSelect(initSecretsManagementFieldDesktopVault, "1Password vault", "Vaults available in the selected 1Password account. Choose manual entry if this list is incomplete.", desktopDiscovery.VaultOptions(initOnePasswordManualSelection), initOnePasswordManualSelection)
	document.addEditableInput(initSecretsManagementFieldDesktopAccountURL, "1Password account URL", "Account sign-in address such as myorg.1password.com for organizational 1Password accounts, or my.1password.com for personal or family accounts. Required only when desktop discovery is unavailable or manual entry is selected.", "", validateOptionalDisplayName)
	document.addEditableInput(initSecretsManagementFieldDesktopAccountID, "1Password account id (advanced)", "Advanced. Optional account id when you need to pin this profile to one signed-in 1Password desktop account.", "", validateOptionalDisplayName)
	document.addEditableInput(initSecretsManagementFieldVaultID, "1Password vault name or id", "Required for every 1Password-backed credential store. Enter a vault name such as Personal, Employee, or My Vault, or enter a stable vault ID.", "", nil)
	document.addEditableInput(
		initSecretsManagementFieldLabel,
		"Credential store name",
		"Choose a human-friendly name for this credential store.",
		"",
		validateOptionalDisplayName,
	)
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
			if id == initSecretsManagementFieldBackend || id == initSecretsManagementFieldDesktopAccount || id == initSecretsManagementFieldDesktopVault {
				initSecretsManagementSyncLinearFields(model, cfg, pendingDeletes, pendingDeleteOrder, desktopDiscovery, false)
			}
		},
		OnEnter: initLinearActionEnterHandler(initSecretsManagementFieldAction, func(model *initLinearEditorModel, action string) (string, error) {
			model.document[model.focused].Error = ""
			switch action {
			case initDetailActionBack:
				return initDetailActionBack, nil
			case initDetailActionEdit:
				if _, err := initSecretsManagementEditFromDocumentWithDiscovery(cfg, model.document, desktopDiscovery); err != nil {
					return "", err
				}
				return initDetailActionEdit, nil
			default:
				return "", nil
			}
		}),
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
		if row.ID == initBackSelection || row.ID == config.LocalOSCredentialStoreID || !row.Selectable {
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
		options = append(options, huh.NewOption(initPendingDeleteLabel(initSecretsStorePendingDeleteTitle(id, pending.Profile)), initLinearRestoreSelection("secrets_management", id)))
	}
	return dedupeInitStringOptions(options)
}

func initSecretsManagementBuiltInSectionDescription() string {
	description := strings.TrimSpace(initBuiltInOSCredentialStoreDescription())
	prefix := "These built-in credential stores are always available and cannot be deleted:\n\n"
	if description == "" {
		return prefix + initBuiltInOSCredentialStoreTitle()
	}
	return fmt.Sprintf("%s%s        %s", prefix, initBuiltInOSCredentialStoreTitle(), description)
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
	Profile  config.SecretsStore
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
	if kind, ok := initSecretsStoreSelectionKind(selection); ok {
		return initSecretsManagementSelectionState{
			Profile:  config.SecretsStore{Backend: normalizeInitSecretsStoreBackend(config.SecretsStoreBackend{Kind: kind})},
			Creating: true,
		}, nil
	}
	if id, ok := initLinearRestoreSelectionName("secrets_management", selection); ok {
		return initSecretsManagementSelectionState{ID: id, Pending: true}, nil
	}
	profile, ok := cfg.Secrets.Stores[selection]
	if !ok {
		return initSecretsManagementSelectionState{}, fmt.Errorf("%w: %s", config.ErrSecretsStoreNotFound, selection)
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
		profile.Backend = normalizeInitSecretsStoreBackend(config.SecretsStoreBackend{Kind: config.SecretsBackendKind(credstore.BackendKeychain)})
	} else {
		profile.Backend = normalizeInitSecretsStoreBackend(profile.Backend)
	}
	if resetDetails {
		labelSeed := initSecretsStoreEditorLabelSeed(profile, state.ID, profile.Backend.Kind, state.Creating)
		model.setFieldValue(initSecretsManagementFieldLabel, labelSeed.DisplayValue)
		initSecretsManagementSetBackendOptions(model, profile.Backend.Kind, state.Creating)
		model.selectFieldValue(initSecretsManagementFieldBackend, string(profile.Backend.Kind))
		onePassword := config.SecretsStoreOnePasswordConfig{}
		if profile.Backend.OnePassword != nil {
			onePassword = *profile.Backend.OnePassword
		}
		model.setFieldValue(initSecretsManagementFieldVaultID, onePassword.VaultID)
		model.setFieldValue(initSecretsManagementFieldTimeout, onePassword.Timeout)
		model.setFieldValue(initSecretsManagementFieldDesktopAccountURL, onePassword.AccountURL)
		accountID := onePassword.AccountID
		model.setFieldValue(initSecretsManagementFieldDesktopAccountID, accountID)
		accountSelection := desktopDiscovery.AccountSelectionFor(accountID, onePassword.AccountURL)
		vaultSelection := desktopDiscovery.VaultSelectionFor(accountSelection, onePassword.VaultID, onePassword.VaultName)
		initSecretsManagementSetDesktopDiscoveryOptions(model, desktopDiscovery, accountSelection, vaultSelection)
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
		initSecretsManagementSetDesktopDiscoveryOptions(model, desktopDiscovery, model.document.selectedValue(initSecretsManagementFieldDesktopAccount), model.document.selectedValue(initSecretsManagementFieldDesktopVault))
	}
	model.setFieldHidden(initSecretsManagementFieldTimeout, !opService && !opDesktop)
	discoveredDesktop := opDesktop && desktopDiscovery.HasAccountChoices()
	accountSelection := model.document.selectedValue(initSecretsManagementFieldDesktopAccount)
	vaultSelection := model.document.selectedValue(initSecretsManagementFieldDesktopVault)
	manualAccount := opDesktop && (!discoveredDesktop || accountSelection == initOnePasswordManualSelection)
	manualVault := manualAccount || vaultSelection == initOnePasswordManualSelection
	if opDesktop && state.Creating {
		initSecretsManagementSyncDesktopLabel(model, desktopDiscovery)
	}
	model.setFieldHidden(initSecretsManagementFieldDesktopAccount, !discoveredDesktop)
	model.setFieldHidden(initSecretsManagementFieldDesktopVault, !discoveredDesktop || manualAccount)
	model.setFieldHidden(initSecretsManagementFieldDesktopAccountURL, !manualAccount)
	model.setFieldHidden(initSecretsManagementFieldDesktopAccountID, !manualAccount)
	model.setFieldHidden(initSecretsManagementFieldVaultID, !onePassword || (discoveredDesktop && !manualVault))
	if opDesktop {
		model.setFieldDescription(initSecretsManagementFieldDesktopVault, initSecretsManagementDesktopVaultDescription(desktopDiscovery, accountSelection))
	}
}

func initSecretsManagementSetTargetOptions(model *initLinearEditorModel, cfg config.File, pendingDeletes map[string]initPendingSecretsManagementDelete, pendingDeleteOrder []string, selected string) {
	cfg = config.Normalize(cfg)
	initLinearSetSelectionOptions(model, initSecretsManagementFieldTarget, initSecretsManagementTargetOptions(cfg, pendingDeletes, pendingDeleteOrder), selected,
		func(value string) bool {
			_, ok := cfg.Secrets.Stores[value]
			return ok && value != config.LocalOSCredentialStoreID
		},
		func(value string) bool {
			_, ok := initLinearRestoreSelectionName("secrets_management", value)
			return ok
		},
	)
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
	model.document[index].Options = initLinearOptionsFromHuh(initSecretsStoreBackendOptions(current), selected)
}

func initSecretsManagementSetDesktopDiscoveryOptions(model *initLinearEditorModel, discovery initOnePasswordDesktopDiscovery, accountSelection, vaultSelection string) {
	accountIndex := model.document.fieldIndexByID(initSecretsManagementFieldDesktopAccount)
	if accountIndex >= 0 {
		model.document[accountIndex].Options = discovery.LinearAccountOptions(accountSelection)
		accountSelection = model.document.selectedValue(initSecretsManagementFieldDesktopAccount)
	}
	vaultIndex := model.document.fieldIndexByID(initSecretsManagementFieldDesktopVault)
	if vaultIndex >= 0 {
		model.document[vaultIndex].Options = discovery.LinearVaultOptions(accountSelection, vaultSelection)
	}
}

func initSecretsManagementDesktopVaultDescription(discovery initOnePasswordDesktopDiscovery, accountSelection string) string {
	if discovery.HasAccountChoices() {
		if account, ok := discovery.Account(accountSelection); ok {
			if len(account.Vaults) == 0 {
				return fmt.Sprintf("Account: %s. No vaults were discovered; enter a vault manually.", account.Label())
			}
			return fmt.Sprintf("Account: %s. Choose a vault below:", account.Label())
		}
	}
	return "Vaults available in the selected 1Password account. Choose manual entry if this list is incomplete."
}

func initSecretsManagementSyncDesktopLabel(model *initLinearEditorModel, discovery initOnePasswordDesktopDiscovery) {
	account, ok := discovery.Account(model.document.selectedValue(initSecretsManagementFieldDesktopAccount))
	if !ok {
		return
	}
	next := initOnePasswordDesktopCredentialStoreLabel(account)
	if next == "" {
		return
	}
	current := strings.TrimSpace(model.document.fieldValue(initSecretsManagementFieldLabel))
	if current != "" && current != "1Password" && current != next && !discovery.GeneratedDesktopCredentialStoreLabel(current) {
		return
	}
	model.setFieldValue(initSecretsManagementFieldLabel, next)
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

func initSecretsStorePendingDeleteTitle(id string, profile config.SecretsStore) string {
	return initSecretsStoreInventoryTitle(config.EffectiveSecretsStore{
		ID:          id,
		DisplayName: profile.DisplayName,
		Backend:     string(profile.Backend.Kind),
		Source:      config.EffectiveSecretsStoreSourceConfigured,
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
	model.setFieldHidden(initSecretsManagementFieldVaultID, hideOnePassword)
	model.setFieldHidden(initSecretsManagementSectionConnect, hideOnePassword || hideConnect)
	model.setFieldHidden(initSecretsManagementFieldConnectHost, hideOnePassword || hideConnect)
	model.setFieldHidden(initSecretsManagementFieldConnectTokenEnv, hideOnePassword || hideConnect)
	model.setFieldHidden(initSecretsManagementSectionServiceAccount, hideOnePassword || hideService)
	model.setFieldHidden(initSecretsManagementFieldServiceTokenEnv, hideOnePassword || hideService)
	model.setFieldHidden(initSecretsManagementFieldDesktopAccount, hideOnePassword || hideDesktop)
	model.setFieldHidden(initSecretsManagementFieldDesktopVault, hideOnePassword || hideDesktop)
	model.setFieldHidden(initSecretsManagementFieldDesktopAccountURL, hideOnePassword || hideDesktop)
	model.setFieldHidden(initSecretsManagementFieldDesktopAccountID, hideOnePassword || hideDesktop)
	model.setFieldHidden(initSecretsManagementFieldTimeout, hideOnePassword)
}

func initSecretsManagementEditFromDocument(cfg config.File, document initLinearDocument) (initKeyringBackendEdit, error) {
	return initSecretsManagementEditFromDocumentWithDiscovery(cfg, document, initOnePasswordDesktopDiscovery{})
}

func initSecretsManagementDeleteEditFromDocument(cfg config.File, document initLinearDocument) (initKeyringBackendEdit, error) {
	state, err := initSecretsManagementSelectionStateForDocument(cfg, document)
	if err != nil {
		return initKeyringBackendEdit{}, err
	}
	if state.Creating || state.BuiltIn || state.Pending || state.ID == "" {
		return initKeyringBackendEdit{}, fmt.Errorf("only configured credential stores can be deleted")
	}
	working := config.Normalize(cloneInitConfigFile(cfg))
	nextCfg, _, err := configedit.RemoveSecretsStore(working, state.ID)
	if err != nil {
		return initKeyringBackendEdit{}, err
	}
	return initKeyringBackendEdit{Apply: true, HasConfigEdit: true, Config: nextCfg}, nil
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
	edit, err := initSecretsManagementProfileEditFromDocument(state, document, desktopDiscovery)
	if err != nil {
		return initKeyringBackendEdit{}, err
	}
	if state.Creating {
		id := initSecretsStoreIDFromLabel(edit.StoredLabel, edit.Backend.Kind, working.Secrets.Stores)
		patch := configedit.SecretsStorePatch{Backend: &edit.Backend}
		if edit.StoredLabel != "" {
			label := edit.StoredLabel
			patch.Label = &label
		}
		nextCfg, _, _, err := configedit.SetSecretsStore(working, id, patch)
		if err != nil {
			return initKeyringBackendEdit{}, err
		}
		return initKeyringBackendEdit{Apply: true, HasConfigEdit: true, Config: nextCfg}, nil
	}
	patch := configedit.SecretsStorePatch{Backend: &edit.Backend}
	if edit.StoredLabel != "" {
		label := edit.StoredLabel
		patch.Label = &label
	} else {
		patch.ClearLabel = true
	}
	nextCfg, _, _, err := configedit.SetSecretsStore(working, state.ID, patch)
	if err != nil {
		return initKeyringBackendEdit{}, err
	}
	return initKeyringBackendEdit{Apply: true, HasConfigEdit: true, Config: nextCfg}, nil
}

func initSecretsManagementProfileEditFromDocument(state initSecretsManagementSelectionState, document initLinearDocument, desktopDiscovery initOnePasswordDesktopDiscovery) (initSecretsStoreEditorResult, error) {
	profile := state.Profile
	if strings.TrimSpace(string(profile.Backend.Kind)) == "" {
		profile.Backend = normalizeInitSecretsStoreBackend(config.SecretsStoreBackend{Kind: config.SecretsBackendKind(credstore.BackendKeychain)})
	} else {
		profile.Backend = normalizeInitSecretsStoreBackend(profile.Backend)
	}
	labelSeed := initSecretsStoreEditorLabelSeed(profile, state.ID, profile.Backend.Kind, state.Creating)
	labelInput := document.fieldValue(initSecretsManagementFieldLabel)
	if err := validateOptionalDisplayName(labelInput); err != nil {
		return initSecretsStoreEditorResult{}, err
	}
	kindValue := document.selectedValue(initSecretsManagementFieldBackend)
	kind := config.SecretsBackendKind(kindValue)
	vaultValue := document.fieldValue(initSecretsManagementFieldVaultID)
	vaultName := ""
	accountID := document.fieldValue(initSecretsManagementFieldDesktopAccountID)
	accountURL := document.fieldValue(initSecretsManagementFieldDesktopAccountURL)
	accountSelection := document.selectedValue(initSecretsManagementFieldDesktopAccount)
	vaultSelection := document.selectedValue(initSecretsManagementFieldDesktopVault)
	discoveredDesktop := kind == config.SecretsBackendKind(credstore.BackendOPDesktop) && desktopDiscovery.HasVaultChoices()
	if discoveredDesktop && accountSelection != initOnePasswordManualSelection {
		if selection, ok := desktopDiscovery.AccountSelection(accountSelection); ok {
			accountID = selection.AccountID
			accountURL = selection.AccountURL
		}
		if vaultSelection != initOnePasswordManualSelection {
			if selection, ok := desktopDiscovery.AccountVaultSelection(accountSelection, vaultSelection); ok {
				accountID = selection.AccountID
				accountURL = selection.AccountURL
				vaultValue = selection.VaultID
				vaultName = selection.VaultName
			}
		}
	}
	if config.IsOnePasswordSecretsBackend(kind) {
		requiresVault := true
		if discoveredDesktop && accountSelection != initOnePasswordManualSelection && vaultSelection != initOnePasswordManualSelection {
			requiresVault = false
		}
		if err := validateInitSecretsRequiredSingleLine(vaultValue, requiresVault, "1Password vault name or id"); err != nil {
			return initSecretsStoreEditorResult{}, err
		}
	}
	if kind == config.SecretsBackendKind(credstore.BackendOPConnect) {
		if err := validateInitSecretsRequiredSingleLine(document.fieldValue(initSecretsManagementFieldConnectHost), true, "1Password Connect host"); err != nil {
			return initSecretsStoreEditorResult{}, err
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
			return initSecretsStoreEditorResult{}, err
		}
	}
	backend := initSecretsStoreBackendFromInputs(initSecretsStoreBackendInput{
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
	return initSecretsStoreEditorResult{
		Apply:       true,
		Label:       strings.TrimSpace(labelInput),
		StoredLabel: normalizeInitSecretsStoreStoredLabel(labelInput, labelSeed.FallbackValue, labelSeed.StoredLabel, state.Creating),
		Backend:     backend,
	}, nil
}

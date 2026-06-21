package credentialcmd

import (
	"fmt"
	"reflect"
	"runtime"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/open-cli-collective/cli-common/credstore"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/configedit"
)

const (
	// #nosec G101 -- selection sentinel, not a credential.
	initConfigureSecretsProfileSelectionPrefix = "__configure_secrets_profile__:"
)

type initSecretsBackendPresentation struct {
	Kind             config.SecretsBackendKind
	Label            string
	Description      string
	Available        bool
	LegacyCompatible bool
}

type initSecretsProfileEditorResult struct {
	Apply       bool
	Label       string
	StoredLabel string
	Backend     config.SecretsProfileBackend
}

type initSecretsProfileBackendInput struct {
	KindValue       string
	Timeout         string
	AccountID       string
	AccountURL      string
	VaultID         string
	VaultName       string
	ConnectHost     string
	ConnectTokenEnv string
	ServiceTokenEnv string
}

func initSecretsBackendCatalog() []initSecretsBackendPresentation {
	order := []config.SecretsBackendKind{
		config.SecretsBackendKind(credstore.BackendOPDesktop),
		config.SecretsBackendKind(credstore.BackendOP),
		config.SecretsBackendKind(credstore.BackendOPConnect),
		config.SecretsBackendKind(credstore.BackendPass),
		config.SecretsBackendKind(credstore.BackendFile),
		config.SecretsBackendKind(credstore.BackendMemory),
	}
	items := make([]initSecretsBackendPresentation, 0, len(order))
	for _, kind := range order {
		items = append(items, initSecretsBackendPresentation{
			Kind:             kind,
			Label:            initSecretsBackendDisplayLabel(kind),
			Description:      initSecretsBackendDescription(kind),
			Available:        initSecretsBackendAvailable(kind),
			LegacyCompatible: !config.IsOnePasswordSecretsBackend(kind),
		})
	}
	return items
}

func initSecretsBackendDisplayLabel(kind config.SecretsBackendKind) string {
	switch kind {
	case config.SecretsBackendKind(credstore.BackendKeychain):
		return "macOS Keychain"
	case config.SecretsBackendKind(credstore.BackendWinCred):
		return "Windows Credential Manager"
	case config.SecretsBackendKind(credstore.BackendSecretService):
		return "Linux Secret Service"
	case config.SecretsBackendKind(credstore.BackendFile):
		return "Encrypted file"
	case config.SecretsBackendKind(credstore.BackendPass):
		return "pass password store"
	case config.SecretsBackendKind(credstore.BackendOP):
		return "1Password service account"
	case config.SecretsBackendKind(credstore.BackendOPConnect):
		return "1Password Connect"
	case config.SecretsBackendKind(credstore.BackendOPDesktop):
		return "1Password desktop app"
	case config.SecretsBackendKind(credstore.BackendMemory):
		return "In-memory store"
	default:
		return string(kind)
	}
}

func initSecretsBackendDescription(kind config.SecretsBackendKind) string {
	switch kind {
	case config.SecretsBackendKind(credstore.BackendKeychain):
		return "Use the signed macOS keychain integration for stored credentials."
	case config.SecretsBackendKind(credstore.BackendWinCred):
		return "Use Windows Credential Manager for stored credentials."
	case config.SecretsBackendKind(credstore.BackendSecretService):
		return "Use the Linux Secret Service keyring for stored credentials."
	case config.SecretsBackendKind(credstore.BackendFile):
		return "Store encrypted credentials on disk with a passphrase."
	case config.SecretsBackendKind(credstore.BackendPass):
		return "Store credentials in an initialized pass password store."
	case config.SecretsBackendKind(credstore.BackendOP):
		return "Use 1Password service-account access. Best for CI or server environments where cr can read a service-account token from an environment variable; requires a vault name or id and service token env var."
	case config.SecretsBackendKind(credstore.BackendOPConnect):
		return "Use a 1Password Connect server. Best when your team runs a Connect API endpoint; requires a vault name or id, Connect host, and Connect token env var."
	case config.SecretsBackendKind(credstore.BackendOPDesktop):
		return "Use the local 1Password desktop app integration. Most common for local use; best for interactive developer machines with an unlocked desktop app; requires a vault name or id and can optionally pin a desktop account id."
	case config.SecretsBackendKind(credstore.BackendMemory):
		return "Keep credentials in memory only for this process. Ephemeral; best suited for tests or CI, not normal local use."
	default:
		return ""
	}
}

func initSecretsBackendAvailable(kind config.SecretsBackendKind) bool {
	switch kind {
	case config.SecretsBackendKind(credstore.BackendKeychain):
		return runtime.GOOS == "darwin"
	case config.SecretsBackendKind(credstore.BackendWinCred):
		return runtime.GOOS == "windows"
	case config.SecretsBackendKind(credstore.BackendSecretService):
		return runtime.GOOS == "linux"
	case config.SecretsBackendKind(credstore.BackendPass):
		return runtime.GOOS != "windows"
	case config.SecretsBackendKind(credstore.BackendOP),
		config.SecretsBackendKind(credstore.BackendOPConnect),
		config.SecretsBackendKind(credstore.BackendOPDesktop):
		return initOnePasswordBackendsAvailable()
	default:
		return true
	}
}

func initSecretsBackendByKind(kind config.SecretsBackendKind) (initSecretsBackendPresentation, bool) {
	for _, item := range initSecretsBackendCatalog() {
		if item.Kind == kind {
			return item, true
		}
	}
	return initSecretsBackendPresentation{}, false
}

func initSecretsManagementInventoryDescription() string {
	return "Choose where cr stores credentials. The built-in OS credential store is always available; configured stores are reusable destinations for secrets."
}

func initBuiltInOSCredentialStoreTitle() string {
	title, _ := initBuiltInOSCredentialStoreLabelsForGOOS(runtime.GOOS)
	return title
}

func initBuiltInOSCredentialStoreDescription() string {
	_, description := initBuiltInOSCredentialStoreLabelsForGOOS(runtime.GOOS)
	return description
}

func initBuiltInOSCredentialStoreLabelsForGOOS(goos string) (string, string) {
	switch goos {
	case "darwin":
		return "macOS Login Keychain", "current macOS user"
	case "windows":
		return "Windows Credential Manager", "current Windows user"
	case "linux":
		return "Linux Secret Service", "current Linux user"
	default:
		return "OS credential store", "current OS user"
	}
}

func initAutomaticOSDefaultSecretsBackendLabel() string {
	return initBuiltInOSCredentialStoreTitle()
}

func initAutomaticOSDefaultSecretsBackendLabelForGOOS(goos string) string {
	title, _ := initBuiltInOSCredentialStoreLabelsForGOOS(goos)
	return title
}

func initSecretsManagementInventoryRows(cfg config.File) []initInventoryRow {
	effective := config.EffectiveSecretsStores(cfg)
	rows := make([]initInventoryRow, 0, len(effective)+len(initSecretsBackendCatalog())+2)
	for _, store := range effective {
		if store.ID == config.LocalOSCredentialStoreID {
			rows = append(rows, initInventoryRow{
				ID:          store.ID,
				Title:       initBuiltInOSCredentialStoreTitle(),
				Description: initBuiltInOSCredentialStoreDescription(),
				Kind:        initInventoryRowKindActive,
				Selectable:  true,
				FilterValue: strings.TrimSpace(strings.Join([]string{store.ID, initBuiltInOSCredentialStoreTitle(), initBuiltInOSCredentialStoreDescription()}, " ")),
			})
			continue
		}
		title := initSecretsProfileInventoryTitle(store)
		rows = append(rows, initInventoryRow{
			ID:          store.ID,
			Title:       title,
			Kind:        initInventoryRowKindActive,
			Selectable:  true,
			Deletable:   true,
			FilterValue: strings.TrimSpace(strings.Join([]string{store.ID, store.DisplayName, store.Backend, title}, " ")),
		})
	}

	for _, backend := range initSecretsBackendCatalog() {
		title := initConfigureSecretsStoreTitle(backend.Kind)
		desc := backend.Description
		if !backend.Available {
			desc = strings.TrimSpace(strings.Join([]string{desc, "Unavailable in this build."}, " "))
		}
		rows = append(rows, initInventoryRow{
			ID:            initConfigureSecretsProfileSelectionPrefix + string(backend.Kind),
			Title:         title,
			Description:   desc,
			Kind:          initInventoryRowKindCommand,
			PrimaryAction: initInventoryActionCommand,
			Selectable:    backend.Available,
			FilterValue:   strings.TrimSpace(strings.Join([]string{string(backend.Kind), initSecretsBackendDisplayLabel(backend.Kind), title}, " ")),
		})
	}
	rows = append(rows, initInventoryRow{
		ID:            initBackSelection,
		Title:         "Back to main menu",
		Kind:          initInventoryRowKindCommand,
		PrimaryAction: initInventoryActionBack,
		Selectable:    true,
	})
	return rows
}

func initConfigureSecretsStoreTitle(kind config.SecretsBackendKind) string {
	switch kind {
	case config.SecretsBackendKind(credstore.BackendOPDesktop):
		return "Configure new 1Password desktop app profile"
	case config.SecretsBackendKind(credstore.BackendOP):
		return "Configure new 1Password service account profile"
	case config.SecretsBackendKind(credstore.BackendOPConnect):
		return "Configure new 1Password Connect profile"
	case config.SecretsBackendKind(credstore.BackendPass):
		return "Configure new pass password store profile"
	case config.SecretsBackendKind(credstore.BackendFile):
		return "Configure new encrypted file profile"
	case config.SecretsBackendKind(credstore.BackendMemory):
		return "Configure new in-memory store profile"
	default:
		return fmt.Sprintf("Configure new %s profile", strings.ToLower(initSecretsBackendDisplayLabel(kind)))
	}
}

func initSecretsProfileInventoryTitle(profile config.EffectiveSecretsProfile) string {
	backendLabel := profile.Backend
	if item, ok := initSecretsBackendByKind(config.SecretsBackendKind(profile.Backend)); ok {
		backendLabel = item.Label
	} else if profile.Backend != "" {
		backendLabel = initSecretsBackendDisplayLabel(config.SecretsBackendKind(profile.Backend))
	}
	title := fmt.Sprintf("%s (%s)", initSecretsProfileDisplayName(profile.ID, profile.Label), backendLabel)
	return title
}

func initSecretsProfileDisplayName(id string, label string) string {
	if trimmed := strings.TrimSpace(label); trimmed != "" {
		return trimmed
	}
	return strings.TrimSpace(id)
}

func initSecretsProfileSelectionKind(selection string) (config.SecretsBackendKind, bool) {
	if !strings.HasPrefix(selection, initConfigureSecretsProfileSelectionPrefix) {
		return "", false
	}
	return config.SecretsBackendKind(strings.TrimPrefix(selection, initConfigureSecretsProfileSelectionPrefix)), true
}

type initSecretsProfileLabelSeed struct {
	DisplayValue  string
	StoredLabel   string
	FallbackValue string
}

func initSecretsProfileEditorLabelSeed(profile config.SecretsProfile, id string, kind config.SecretsBackendKind, creating bool) initSecretsProfileLabelSeed {
	if trimmed := strings.TrimSpace(profile.Label); trimmed != "" {
		return initSecretsProfileLabelSeed{
			DisplayValue: trimmed,
			StoredLabel:  trimmed,
		}
	}
	if !creating && strings.TrimSpace(id) != "" {
		fallback := strings.TrimSpace(id)
		return initSecretsProfileLabelSeed{
			DisplayValue:  fallback,
			FallbackValue: fallback,
		}
	}
	fallback := initSecretsBackendDisplayLabel(kind)
	if creating && kind == config.SecretsBackendKind(credstore.BackendOPDesktop) {
		fallback = "1Password"
	}
	return initSecretsProfileLabelSeed{
		DisplayValue:  fallback,
		FallbackValue: fallback,
	}
}

func initSecretsProfileBackendOptions(current config.SecretsBackendKind) []huh.Option[string] {
	options := make([]huh.Option[string], 0, len(initSecretsBackendCatalog()))
	for _, backend := range initSecretsBackendCatalog() {
		if !backend.Available && backend.Kind != current {
			continue
		}
		label := backend.Label
		if !backend.Available && backend.Kind == current {
			label += " (unavailable in this build; existing config)"
		}
		options = append(options, huh.NewOption(label, string(backend.Kind)))
	}
	return options
}

func initSecretsProfileBackendFromInputs(input initSecretsProfileBackendInput) config.SecretsProfileBackend {
	backend := config.SecretsProfileBackend{
		Kind: config.SecretsBackendKind(strings.TrimSpace(input.KindValue)),
	}
	if !config.IsOnePasswordSecretsBackend(backend.Kind) {
		return normalizeInitSecretsProfileBackend(backend)
	}
	backend.OnePassword = &config.SecretsProfileOnePasswordConfig{
		Timeout:         strings.TrimSpace(input.Timeout),
		AccountID:       strings.TrimSpace(input.AccountID),
		AccountURL:      strings.TrimSpace(input.AccountURL),
		VaultID:         strings.TrimSpace(input.VaultID),
		VaultName:       strings.TrimSpace(input.VaultName),
		ConnectHost:     strings.TrimSpace(input.ConnectHost),
		ConnectTokenEnv: strings.TrimSpace(input.ConnectTokenEnv),
		ServiceTokenEnv: strings.TrimSpace(input.ServiceTokenEnv),
	}
	return normalizeInitSecretsProfileBackend(backend)
}

func normalizeInitSecretsProfileStoredLabel(labelInput string, fallbackSeed string, explicitExistingLabel string, creating bool) string {
	label := strings.TrimSpace(labelInput)
	if label == "" {
		return ""
	}
	if creating {
		return label
	}
	userKeptAutogeneratedLabel := strings.TrimSpace(explicitExistingLabel) == "" && label == strings.TrimSpace(fallbackSeed)
	if userKeptAutogeneratedLabel {
		return ""
	}
	return label
}

func initSecretsProfileIDFromLabel(label string, kind config.SecretsBackendKind, existing map[string]config.SecretsProfile) string {
	base := normalizeInitSecretsProfileIDToken(label)
	if base == "" {
		base = normalizeInitSecretsProfileIDToken(initSecretsBackendDisplayLabel(kind))
	}
	if base == "" {
		base = "secrets-profile"
	}
	if base == config.LocalOSCredentialStoreID {
		base = "secrets-profile"
	}
	candidate := base
	for suffix := 2; ; suffix++ {
		if _, ok := existing[candidate]; !ok {
			return candidate
		}
		candidate = fmt.Sprintf("%s-%d", base, suffix)
	}
}

func normalizeInitSecretsProfileIDToken(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return ""
	}
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func normalizeInitSecretsProfileBackend(backend config.SecretsProfileBackend) config.SecretsProfileBackend {
	working := config.File{
		Profiles: map[string]config.Profile{"default": {}},
		Secrets: config.SecretsConfig{
			Profiles: map[string]config.SecretsProfile{
				"seed": {Backend: backend},
			},
		},
		DefaultProfile: "default",
	}
	return config.Normalize(working).Secrets.Profiles["seed"].Backend
}

func initConfigsEqual(a, b config.File) bool {
	return reflect.DeepEqual(config.Normalize(a), config.Normalize(b))
}

func validateInitSecretsRequiredSingleLine(value string, required bool, field string) error {
	trimmed := strings.TrimSpace(value)
	if required && trimmed == "" {
		return fmt.Errorf("%s is required", field)
	}
	return validateOptionalDisplayName(trimmed)
}

func (p huhInitKeyringBackendPrompter) runInventory(prompt initInventoryPrompt) (initInventoryResult, error) {
	runner := p.inventoryRunner
	if runner == nil {
		runner = runInitInventory
	}
	return runner(prompt, p.stdin, p.stderr)
}

func (p huhInitKeyringBackendPrompter) EditKeyringBackend(prompt initKeyringBackendPrompt) (initKeyringBackendEdit, error) {
	if p.inventoryRunner == nil {
		return p.editKeyringBackendLinear(prompt)
	}
	working := config.Normalize(cloneInitConfigFile(prompt.Config))
	original := config.Normalize(cloneInitConfigFile(prompt.Config))

	for {
		result, err := p.runInventory(initInventoryPrompt{
			Title:       "Secrets Storage",
			Description: initSecretsManagementInventoryDescription(),
			Rows:        initSecretsManagementInventoryRows(working),
			Width:       88,
			Height:      18,
		})
		if err != nil {
			return initKeyringBackendEdit{}, err
		}
		switch result.Action {
		case initInventoryActionBack:
			if initConfigsEqual(original, working) {
				return initKeyringBackendEdit{}, errInitNavigateBack
			}
			return initKeyringBackendEdit{Apply: true, HasConfigEdit: true, Config: working}, nil
		case initInventoryActionRestore, initInventoryActionStageDelete:
			return initKeyringBackendEdit{}, fmt.Errorf("unsupported secrets-management inventory action %q", result.Action)
		case initInventoryActionCommand, initInventoryActionEdit:
			switch {
			case result.Row.ID == config.LocalOSCredentialStoreID:
				continue
			case strings.HasPrefix(result.Row.ID, initConfigureSecretsProfileSelectionPrefix):
				kind, ok := initSecretsProfileSelectionKind(result.Row.ID)
				if !ok {
					return initKeyringBackendEdit{}, fmt.Errorf("invalid secrets-management selection %q", result.Row.ID)
				}
				edit, err := p.editSecretsProfile(config.SecretsProfile{
					Backend: normalizeInitSecretsProfileBackend(config.SecretsProfileBackend{Kind: kind}),
				}, "", false, true)
				if err != nil {
					return initKeyringBackendEdit{}, err
				}
				if !edit.Apply {
					continue
				}
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
				working = nextCfg
			case result.Row.ID == initBackSelection:
				if initConfigsEqual(original, working) {
					return initKeyringBackendEdit{}, errInitNavigateBack
				}
				return initKeyringBackendEdit{Apply: true, HasConfigEdit: true, Config: working}, nil
			default:
				working = config.Normalize(working)
				existing, ok := working.Secrets.Stores[result.Row.ID]
				if !ok {
					return initKeyringBackendEdit{}, fmt.Errorf("%w: %s", config.ErrSecretsProfileNotFound, result.Row.ID)
				}
				edit, err := p.editSecretsProfile(existing, result.Row.ID, false, false)
				if err != nil {
					return initKeyringBackendEdit{}, err
				}
				if !edit.Apply {
					continue
				}
				patch := configedit.SecretsProfilePatch{
					Backend: &edit.Backend,
				}
				if edit.StoredLabel != "" {
					label := edit.StoredLabel
					patch.Label = &label
				} else {
					patch.ClearLabel = true
				}
				nextCfg, _, _, err := configedit.SetSecretsProfile(working, result.Row.ID, patch)
				if err != nil {
					return initKeyringBackendEdit{}, err
				}
				working = nextCfg
			}
		case initInventoryActionNone:
			continue
		default:
			return initKeyringBackendEdit{}, fmt.Errorf("unsupported secrets-management inventory action %q", result.Action)
		}
	}
}

func (p huhInitKeyringBackendPrompter) editSecretsProfile(profile config.SecretsProfile, id string, _ bool, creating bool) (initSecretsProfileEditorResult, error) {
	seedProfile := profile
	if strings.TrimSpace(string(seedProfile.Backend.Kind)) == "" {
		seedProfile.Backend = normalizeInitSecretsProfileBackend(config.SecretsProfileBackend{Kind: config.SecretsBackendKind(credstore.BackendKeychain)})
	} else {
		seedProfile.Backend = normalizeInitSecretsProfileBackend(seedProfile.Backend)
	}
	labelSeed := initSecretsProfileEditorLabelSeed(seedProfile, id, seedProfile.Backend.Kind, creating)
	labelInput := labelSeed.DisplayValue
	kindValue := string(seedProfile.Backend.Kind)
	onePassword := &config.SecretsProfileOnePasswordConfig{}
	if seedProfile.Backend.OnePassword != nil {
		copyValue := *seedProfile.Backend.OnePassword
		onePassword = &copyValue
	}
	action := initDetailActionEdit
	timeout := onePassword.Timeout
	vaultID := onePassword.VaultID
	vaultName := onePassword.VaultName
	accountID := onePassword.AccountID
	accountURL := onePassword.AccountURL
	connectHost := onePassword.ConnectHost
	connectTokenEnv := onePassword.ConnectTokenEnv
	serviceTokenEnv := onePassword.ServiceTokenEnv
	if accountID == "" {
		accountID = onePassword.DesktopAccountID
	}
	desktopDiscovery := initOnePasswordDesktopDiscovery{}
	desktopSelection := initOnePasswordManualSelection
	if seedProfile.Backend.Kind == config.SecretsBackendKind(credstore.BackendOPDesktop) {
		p.writeSecretsStorageDiscoveryNotice()
		desktopDiscovery = p.discoverOnePasswordDesktop()
		p.writeSecretsStorageDiscoveryResults(desktopDiscovery)
		desktopSelection = desktopDiscovery.SelectionFor(accountID, accountURL, vaultID, vaultName)
		if desktopSelection == initOnePasswordManualSelection && creating && accountID == "" && accountURL == "" && vaultID == "" && vaultName == "" {
			if options := desktopDiscovery.Options(); len(options) > 0 {
				desktopSelection = options[0].Value
			}
		}
	}

	groups := []*huh.Group{
		huh.NewGroup(
			huh.NewInput().
				Title("Credential store name").
				Description("Choose a human-friendly name for this credential store.").
				Value(&labelInput).
				Validate(validateOptionalDisplayName),
			huh.NewSelect[string]().
				Title("Credential store backend").
				Options(initSecretsProfileBackendOptions(seedProfile.Backend.Kind)...).
				Value(&kindValue),
		).Title("Credential Store Details"),
	}
	if desktopDiscovery.HasVaultChoices() {
		groups = append(groups, huh.NewGroup(
			huh.NewSelect[string]().
				Title("1Password account and vault").
				Description("Discovered from the local 1Password desktop app. Choose manual entry if this list is incomplete.").
				Options(desktopDiscovery.Options()...).
				Value(&desktopSelection),
		).WithHideFunc(func() bool {
			return config.SecretsBackendKind(kindValue) != config.SecretsBackendKind(credstore.BackendOPDesktop)
		}).Title("1Password Desktop Discovery"))
	}
	groups = append(groups,
		huh.NewGroup(
			huh.NewInput().
				Title("1Password account URL").
				Description("Account sign-in address such as myorg.1password.com for organizational 1Password accounts, or my.1password.com for personal or family accounts. Required only when desktop discovery is unavailable or manual entry is selected.").
				Value(&accountURL).
				Validate(validateOptionalDisplayName),
			huh.NewInput().
				Title("1Password account id").
				Description("Advanced. Optional account id when you need to pin this profile to one signed-in 1Password desktop account.").
				Value(&accountID).
				Validate(validateOptionalDisplayName),
		).WithHideFunc(func() bool {
			return config.SecretsBackendKind(kindValue) != config.SecretsBackendKind(credstore.BackendOPDesktop) || (desktopDiscovery.HasVaultChoices() && desktopSelection != initOnePasswordManualSelection)
		}).Title("1Password Desktop Manual Account"),
		huh.NewGroup(
			huh.NewInput().
				Title("1Password vault name or id").
				Description("Required for every 1Password-backed credential store.").
				Value(&vaultID).
				Validate(func(value string) error {
					requiresVault := config.IsOnePasswordSecretsBackend(config.SecretsBackendKind(kindValue))
					if config.SecretsBackendKind(kindValue) == config.SecretsBackendKind(credstore.BackendOPDesktop) && desktopDiscovery.HasVaultChoices() && desktopSelection != initOnePasswordManualSelection {
						requiresVault = false
					}
					return validateInitSecretsRequiredSingleLine(value, requiresVault, "1Password vault name or id")
				}),
		).WithHideFunc(func() bool {
			if !config.IsOnePasswordSecretsBackend(config.SecretsBackendKind(kindValue)) {
				return true
			}
			return config.SecretsBackendKind(kindValue) == config.SecretsBackendKind(credstore.BackendOPDesktop) && desktopDiscovery.HasVaultChoices() && desktopSelection != initOnePasswordManualSelection
		}).Title("1Password Details"),
		huh.NewGroup(
			huh.NewInput().
				Title("1Password timeout").
				Description("Non-secret timeout for 1Password requests. Leave the default in place unless you need to override it.").
				Value(&timeout).
				Validate(validateOptionalDuration),
		).WithHideFunc(func() bool {
			kind := config.SecretsBackendKind(kindValue)
			return kind != config.SecretsBackendKind(credstore.BackendOP) && kind != config.SecretsBackendKind(credstore.BackendOPDesktop)
		}),
		huh.NewGroup(
			huh.NewInput().
				Title("1Password Connect host").
				Description("Required only for 1Password Connect profiles.").
				Value(&connectHost).
				Validate(func(value string) error {
					return validateInitSecretsRequiredSingleLine(value, config.SecretsBackendKind(kindValue) == config.SecretsBackendKind(credstore.BackendOPConnect), "1Password Connect host")
				}),
		).WithHideFunc(func() bool {
			return config.SecretsBackendKind(kindValue) != config.SecretsBackendKind(credstore.BackendOPConnect)
		}),
		huh.NewGroup(
			huh.NewInput().
				Title("1Password Connect token env var").
				Description("Environment variable that holds the 1Password Connect token.").
				Value(&connectTokenEnv).
				Validate(validateOptionalDisplayName),
		).WithHideFunc(func() bool {
			return config.SecretsBackendKind(kindValue) != config.SecretsBackendKind(credstore.BackendOPConnect)
		}),
		huh.NewGroup(
			huh.NewInput().
				Title("1Password service token env var").
				Description("Environment variable that holds the 1Password service-account token.").
				Value(&serviceTokenEnv).
				Validate(validateOptionalDisplayName),
		).WithHideFunc(func() bool {
			return config.SecretsBackendKind(kindValue) != config.SecretsBackendKind(credstore.BackendOP)
		}),
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Credential store action").
				Options(
					huh.NewOption("Stage secrets-storage settings", initDetailActionEdit),
					huh.NewOption("Back without staging", initDetailActionBack),
				).
				Value(&action),
		).Title("Credential Store Details"),
	)
	form := huh.NewForm(groups...)
	back, err := runBackableInitForm(form, p.stdin, p.stderr)
	if err != nil {
		return initSecretsProfileEditorResult{}, err
	}
	if back || action == initDetailActionBack {
		return initSecretsProfileEditorResult{}, nil
	}
	if config.SecretsBackendKind(kindValue) == config.SecretsBackendKind(credstore.BackendOPDesktop) && desktopDiscovery.HasVaultChoices() && desktopSelection != initOnePasswordManualSelection {
		if selection, ok := desktopDiscovery.Selection(desktopSelection); ok {
			accountID = selection.AccountID
			accountURL = selection.AccountURL
			vaultID = selection.VaultID
			vaultName = selection.VaultName
		}
	}
	backend := initSecretsProfileBackendFromInputs(initSecretsProfileBackendInput{
		KindValue:       kindValue,
		Timeout:         timeout,
		AccountID:       accountID,
		AccountURL:      accountURL,
		VaultID:         vaultID,
		VaultName:       vaultName,
		ConnectHost:     connectHost,
		ConnectTokenEnv: connectTokenEnv,
		ServiceTokenEnv: serviceTokenEnv,
	})
	storedLabel := normalizeInitSecretsProfileStoredLabel(labelInput, labelSeed.FallbackValue, labelSeed.StoredLabel, creating)
	return initSecretsProfileEditorResult{
		Apply:       true,
		Label:       strings.TrimSpace(labelInput),
		StoredLabel: storedLabel,
		Backend:     backend,
	}, nil
}

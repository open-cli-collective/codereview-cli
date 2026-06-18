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
	initSecretsManagementLegacySelection = "__legacy_secrets_management__"
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
	UseDefault  bool
}

type initLegacySecretsManagementEditorResult struct {
	Apply   bool
	Backend string
}

func initSecretsBackendCatalog() []initSecretsBackendPresentation {
	order := []config.SecretsBackendKind{
		config.SecretsBackendKind(credstore.BackendKeychain),
		config.SecretsBackendKind(credstore.BackendWinCred),
		config.SecretsBackendKind(credstore.BackendSecretService),
		config.SecretsBackendKind(credstore.BackendPass),
		config.SecretsBackendKind(credstore.BackendFile),
		config.SecretsBackendKind(credstore.BackendOPDesktop),
		config.SecretsBackendKind(credstore.BackendOP),
		config.SecretsBackendKind(credstore.BackendOPConnect),
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
	return "Choose how cr should store credentials. Secrets-management profiles are reusable store definitions that review profiles can choose later."
}

func initAutomaticOSDefaultSecretsBackendLabel() string {
	return initAutomaticOSDefaultSecretsBackendLabelForGOOS(runtime.GOOS)
}

func initAutomaticOSDefaultSecretsBackendLabelForGOOS(goos string) string {
	switch goos {
	case "darwin":
		return "Automatic OS default (macOS Keychain)"
	case "windows":
		return "Automatic OS default (Windows Credential Manager)"
	case "linux":
		return "Automatic OS default (Linux Secret Service)"
	default:
		return "Automatic OS default"
	}
}

func initSecretsManagementInventoryRows(cfg config.File) []initInventoryRow {
	effective := config.EffectiveSecretsProfiles(cfg)
	rows := make([]initInventoryRow, 0, len(effective)+len(initSecretsBackendCatalog())+2)
	for _, profile := range effective {
		if profile.Source != config.EffectiveSecretsProfileSourceConfigured {
			continue
		}
		title := initSecretsProfileInventoryTitle(profile)
		rows = append(rows, initInventoryRow{
			ID:          profile.ID,
			Title:       title,
			Kind:        initInventoryRowKindActive,
			Selectable:  true,
			FilterValue: strings.TrimSpace(strings.Join([]string{profile.ID, profile.Label, profile.Backend, title}, " ")),
		})
	}
	rows = append(rows, initInventoryRow{
		ID:            initSecretsManagementLegacySelection,
		Title:         initLegacySecretsManagementInventoryTitle(cfg),
		Description:   "Legacy fallback credential store used only by profiles that do not choose a named secrets-management profile.",
		Kind:          initInventoryRowKindActive,
		PrimaryAction: initInventoryActionCommand,
		Selectable:    true,
		FilterValue:   strings.TrimSpace(strings.Join([]string{"fallback compatibility legacy credential store keyring backend", strings.TrimSpace(cfg.Keyring.Backend)}, " ")),
	})

	for _, backend := range initSecretsBackendCatalog() {
		title := fmt.Sprintf("Configure new %s profile", strings.ToLower(initSecretsBackendDisplayLabel(backend.Kind)))
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

func initSecretsProfileInventoryTitle(profile config.EffectiveSecretsProfile) string {
	backendLabel := profile.Backend
	if item, ok := initSecretsBackendByKind(config.SecretsBackendKind(profile.Backend)); ok {
		backendLabel = item.Label
	}
	title := fmt.Sprintf("%s (%s)", initSecretsProfileDisplayName(profile.ID, profile.Label), backendLabel)
	if profile.IsDefault {
		title += " [default]"
	}
	return title
}

func initLegacySecretsManagementInventoryTitle(cfg config.File) string {
	backend := strings.TrimSpace(cfg.Keyring.Backend)
	if backend == "" {
		backend = config.ProjectedLegacySecretsBackendKind
	}
	backendLabel := backend
	if backend != config.ProjectedLegacySecretsBackendKind {
		if item, ok := initSecretsBackendByKind(config.SecretsBackendKind(backend)); ok {
			backendLabel = item.Label
		}
	}
	if backend == config.ProjectedLegacySecretsBackendKind {
		backendLabel = initAutomaticOSDefaultSecretsBackendLabel()
	}
	return fmt.Sprintf("Fallback credential store: %s", backendLabel)
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

func initLegacySecretsBackendOptions(current string) []huh.Option[string] {
	options := []huh.Option[string]{
		huh.NewOption(initAutomaticOSDefaultSecretsBackendLabel(), ""),
	}
	for _, backend := range initSecretsBackendCatalog() {
		if !backend.LegacyCompatible {
			continue
		}
		if !backend.Available && string(backend.Kind) != strings.TrimSpace(current) {
			continue
		}
		label := backend.Label
		if !backend.Available && string(backend.Kind) == strings.TrimSpace(current) {
			label += " (unavailable in this build; existing config)"
		}
		options = append(options, huh.NewOption(label, string(backend.Kind)))
	}
	return options
}

func initSecretsProfileBackendFromInputs(kindValue string, timeout string, vaultID string, itemTitlePrefix string, itemTag string, itemFieldTitle string, connectHost string, connectTokenEnv string, serviceTokenEnv string, desktopAccountID string) config.SecretsProfileBackend {
	backend := config.SecretsProfileBackend{
		Kind: config.SecretsBackendKind(strings.TrimSpace(kindValue)),
	}
	if !config.IsOnePasswordSecretsBackend(backend.Kind) {
		return normalizeInitSecretsProfileBackend(backend)
	}
	backend.OnePassword = &config.SecretsProfileOnePasswordConfig{
		Timeout:          strings.TrimSpace(timeout),
		VaultID:          strings.TrimSpace(vaultID),
		ItemTitlePrefix:  strings.TrimSpace(itemTitlePrefix),
		ItemTag:          strings.TrimSpace(itemTag),
		ItemFieldTitle:   strings.TrimSpace(itemFieldTitle),
		ConnectHost:      strings.TrimSpace(connectHost),
		ConnectTokenEnv:  strings.TrimSpace(connectTokenEnv),
		ServiceTokenEnv:  strings.TrimSpace(serviceTokenEnv),
		DesktopAccountID: strings.TrimSpace(desktopAccountID),
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
	if base == config.LegacyProjectedSecretsProfileID {
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
	working := cloneInitConfigFile(prompt.Config)
	original := cloneInitConfigFile(prompt.Config)

	for {
		result, err := p.runInventory(initInventoryPrompt{
			Title:       "Secrets Management",
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
			case result.Row.ID == initSecretsManagementLegacySelection:
				edit, err := p.editLegacySecretsManagement(working.Keyring.Backend)
				if err != nil {
					return initKeyringBackendEdit{}, err
				}
				if !edit.Apply {
					continue
				}
				working.Keyring.Backend = strings.TrimSpace(edit.Backend)
				working = config.Normalize(working)
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
				if edit.UseDefault {
					nextCfg, _, err = configedit.SetDefaultSecretsProfile(nextCfg, id)
				} else if strings.TrimSpace(working.Secrets.DefaultProfile) == id {
					nextCfg, _, err = configedit.UnsetDefaultSecretsProfile(nextCfg)
				}
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
				existing, ok := working.Secrets.Profiles[result.Row.ID]
				if !ok {
					return initKeyringBackendEdit{}, fmt.Errorf("%w: %s", config.ErrSecretsProfileNotFound, result.Row.ID)
				}
				edit, err := p.editSecretsProfile(existing, result.Row.ID, strings.TrimSpace(working.Secrets.DefaultProfile) == result.Row.ID, false)
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
				if edit.UseDefault {
					nextCfg, _, err = configedit.SetDefaultSecretsProfile(nextCfg, result.Row.ID)
				} else if strings.TrimSpace(working.Secrets.DefaultProfile) == result.Row.ID {
					nextCfg, _, err = configedit.UnsetDefaultSecretsProfile(nextCfg)
				}
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

func (p huhInitKeyringBackendPrompter) editSecretsProfile(profile config.SecretsProfile, id string, isDefault bool, creating bool) (initSecretsProfileEditorResult, error) {
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
	useDefault := isDefault
	timeout := onePassword.Timeout
	vaultID := onePassword.VaultID
	itemTitlePrefix := onePassword.ItemTitlePrefix
	itemTag := onePassword.ItemTag
	itemFieldTitle := onePassword.ItemFieldTitle
	connectHost := onePassword.ConnectHost
	connectTokenEnv := onePassword.ConnectTokenEnv
	serviceTokenEnv := onePassword.ServiceTokenEnv
	desktopAccountID := onePassword.DesktopAccountID

	form := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Secrets-management profile label").
				Description("Choose a human-friendly label for this secrets-management profile. This is what the init menus will show later.").
				Value(&labelInput).
				Validate(validateOptionalDisplayName),
			huh.NewSelect[string]().
				Title("Secrets-management backend").
				Options(initSecretsProfileBackendOptions(seedProfile.Backend.Kind)...).
				Value(&kindValue),
		).Title("Secrets Management Profile Details"),
		huh.NewGroup(
			huh.NewInput().
				Title("1Password vault id").
				Description("Required for every 1Password-backed secrets-management profile.").
				Value(&vaultID).
				Validate(func(value string) error {
					return validateInitSecretsRequiredSingleLine(value, config.IsOnePasswordSecretsBackend(config.SecretsBackendKind(kindValue)), "1Password vault id")
				}),
		).WithHideFunc(func() bool {
			return !config.IsOnePasswordSecretsBackend(config.SecretsBackendKind(kindValue))
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
				Title("1Password item title prefix").
				Description("Optional prefix added to stored 1Password item titles.").
				Value(&itemTitlePrefix).
				Validate(validateOptionalDisplayName),
		).WithHideFunc(func() bool {
			return !config.IsOnePasswordSecretsBackend(config.SecretsBackendKind(kindValue))
		}),
		huh.NewGroup(
			huh.NewInput().
				Title("1Password item tag").
				Description("Optional 1Password item tag for credentials created through this profile.").
				Value(&itemTag).
				Validate(validateOptionalDisplayName),
		).WithHideFunc(func() bool {
			return !config.IsOnePasswordSecretsBackend(config.SecretsBackendKind(kindValue))
		}),
		huh.NewGroup(
			huh.NewInput().
				Title("1Password item field title").
				Description("Optional 1Password field title override.").
				Value(&itemFieldTitle).
				Validate(validateOptionalDisplayName),
		).WithHideFunc(func() bool {
			return !config.IsOnePasswordSecretsBackend(config.SecretsBackendKind(kindValue))
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
			huh.NewInput().
				Title("1Password desktop account id").
				Description("Optional desktop-account id when you want to pin this profile to one 1Password desktop account.").
				Value(&desktopAccountID).
				Validate(validateOptionalDisplayName),
		).WithHideFunc(func() bool {
			return config.SecretsBackendKind(kindValue) != config.SecretsBackendKind(credstore.BackendOPDesktop)
		}),
		huh.NewGroup(
			huh.NewSelect[bool]().
				Title("Use this as the default secrets-management profile").
				Options(
					huh.NewOption("No", false),
					huh.NewOption("Yes", true),
				).
				Value(&useDefault),
			huh.NewSelect[string]().
				Title("Secrets-management detail action").
				Options(
					huh.NewOption("Stage secrets-management settings", initDetailActionEdit),
					huh.NewOption("Back without staging", initDetailActionBack),
				).
				Value(&action),
		).Title("Secrets Management Profile Details"),
	)
	back, err := runBackableInitForm(form, p.stdin, p.stderr)
	if err != nil {
		return initSecretsProfileEditorResult{}, err
	}
	if back || action == initDetailActionBack {
		return initSecretsProfileEditorResult{}, nil
	}
	backend := initSecretsProfileBackendFromInputs(kindValue, timeout, vaultID, itemTitlePrefix, itemTag, itemFieldTitle, connectHost, connectTokenEnv, serviceTokenEnv, desktopAccountID)
	storedLabel := normalizeInitSecretsProfileStoredLabel(labelInput, labelSeed.FallbackValue, labelSeed.StoredLabel, creating)
	return initSecretsProfileEditorResult{
		Apply:       true,
		Label:       strings.TrimSpace(labelInput),
		StoredLabel: storedLabel,
		Backend:     backend,
		UseDefault:  useDefault,
	}, nil
}

func (p huhInitKeyringBackendPrompter) editLegacySecretsManagement(currentBackend string) (initLegacySecretsManagementEditorResult, error) {
	backend := strings.TrimSpace(currentBackend)
	action := initDetailActionEdit
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Legacy fallback credential store backend").
				Options(initLegacySecretsBackendOptions(currentBackend)...).
				Value(&backend),
			huh.NewSelect[string]().
				Title("Fallback credential-store action").
				Options(
					huh.NewOption("Stage fallback credential-store settings", initDetailActionEdit),
					huh.NewOption("Back without staging", initDetailActionBack),
				).
				Value(&action),
		).Title("Legacy Fallback Credential Store"),
	)
	back, err := runBackableInitForm(form, p.stdin, p.stderr)
	if err != nil {
		return initLegacySecretsManagementEditorResult{}, err
	}
	if back || action == initDetailActionBack {
		return initLegacySecretsManagementEditorResult{}, nil
	}
	return initLegacySecretsManagementEditorResult{Apply: true, Backend: strings.TrimSpace(backend)}, nil
}

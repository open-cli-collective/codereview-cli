package initcmd

import (
	"fmt"
	"reflect"
	"runtime"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/open-cli-collective/cli-common/credstore"

	"github.com/open-cli-collective/codereview-cli/internal/config"
)

const (
	// #nosec G101 -- selection sentinel, not a credential.
	initConfigureSecretsStoreSelectionPrefix = "__configure_secrets_profile__:"
)

type initSecretsBackendPresentation struct {
	Kind             config.SecretsBackendKind
	Label            string
	Description      string
	Available        bool
	LegacyCompatible bool
}

type initSecretsStoreEditorResult struct {
	Apply       bool
	Label       string
	StoredLabel string
	Backend     config.SecretsStoreBackend
}

type initSecretsStoreBackendInput struct {
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
		title := initSecretsStoreInventoryTitle(store)
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
			ID:            initConfigureSecretsStoreSelectionPrefix + string(backend.Kind),
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

func initSecretsStoreInventoryTitle(profile config.EffectiveSecretsStore) string {
	backendLabel := profile.Backend
	if item, ok := initSecretsBackendByKind(config.SecretsBackendKind(profile.Backend)); ok {
		backendLabel = item.Label
	} else if profile.Backend != "" {
		backendLabel = initSecretsBackendDisplayLabel(config.SecretsBackendKind(profile.Backend))
	}
	title := fmt.Sprintf("%s (%s)", initSecretsStoreDisplayName(profile.ID, profile.DisplayName), backendLabel)
	return title
}

func initSecretsStoreDisplayName(id string, label string) string {
	if trimmed := strings.TrimSpace(label); trimmed != "" {
		return trimmed
	}
	return strings.TrimSpace(id)
}

func initSecretsStoreSelectionKind(selection string) (config.SecretsBackendKind, bool) {
	if !strings.HasPrefix(selection, initConfigureSecretsStoreSelectionPrefix) {
		return "", false
	}
	return config.SecretsBackendKind(strings.TrimPrefix(selection, initConfigureSecretsStoreSelectionPrefix)), true
}

type initSecretsStoreLabelSeed struct {
	DisplayValue  string
	StoredLabel   string
	FallbackValue string
}

func initSecretsStoreEditorLabelSeed(profile config.SecretsStore, id string, kind config.SecretsBackendKind, creating bool) initSecretsStoreLabelSeed {
	if trimmed := strings.TrimSpace(profile.DisplayName); trimmed != "" {
		return initSecretsStoreLabelSeed{
			DisplayValue: trimmed,
			StoredLabel:  trimmed,
		}
	}
	if !creating && strings.TrimSpace(id) != "" {
		fallback := strings.TrimSpace(id)
		return initSecretsStoreLabelSeed{
			DisplayValue:  fallback,
			FallbackValue: fallback,
		}
	}
	fallback := initSecretsBackendDisplayLabel(kind)
	if creating && kind == config.SecretsBackendKind(credstore.BackendOPDesktop) {
		fallback = "1Password"
	}
	return initSecretsStoreLabelSeed{
		DisplayValue:  fallback,
		FallbackValue: fallback,
	}
}

func initSecretsStoreBackendOptions(current config.SecretsBackendKind) []huh.Option[string] {
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

func initSecretsStoreBackendFromInputs(input initSecretsStoreBackendInput) config.SecretsStoreBackend {
	backend := config.SecretsStoreBackend{
		Kind: config.SecretsBackendKind(strings.TrimSpace(input.KindValue)),
	}
	if !config.IsOnePasswordSecretsBackend(backend.Kind) {
		return normalizeInitSecretsStoreBackend(backend)
	}
	backend.OnePassword = &config.SecretsStoreOnePasswordConfig{
		Timeout:         strings.TrimSpace(input.Timeout),
		AccountID:       strings.TrimSpace(input.AccountID),
		AccountURL:      strings.TrimSpace(input.AccountURL),
		VaultID:         strings.TrimSpace(input.VaultID),
		VaultName:       strings.TrimSpace(input.VaultName),
		ConnectHost:     strings.TrimSpace(input.ConnectHost),
		ConnectTokenEnv: strings.TrimSpace(input.ConnectTokenEnv),
		ServiceTokenEnv: strings.TrimSpace(input.ServiceTokenEnv),
	}
	return normalizeInitSecretsStoreBackend(backend)
}

func normalizeInitSecretsStoreStoredLabel(labelInput string, fallbackSeed string, explicitExistingLabel string, creating bool) string {
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

func initSecretsStoreIDFromLabel(label string, kind config.SecretsBackendKind, existing map[string]config.SecretsStore) string {
	base := normalizeInitSecretsStoreIDToken(label)
	if base == "" {
		base = normalizeInitSecretsStoreIDToken(initSecretsBackendDisplayLabel(kind))
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

func normalizeInitSecretsStoreIDToken(value string) string {
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

func normalizeInitSecretsStoreBackend(backend config.SecretsStoreBackend) config.SecretsStoreBackend {
	working := config.File{
		Profiles: map[string]config.Profile{"default": {}},
		Secrets: config.SecretsConfig{
			Stores: map[string]config.SecretsStore{
				"seed": {Backend: backend},
			},
		},
	}
	return config.Normalize(working).Secrets.Stores["seed"].Backend
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

func (p huhInitKeyringBackendPrompter) EditKeyringBackend(prompt initKeyringBackendPrompt) (initKeyringBackendEdit, error) {
	return p.editKeyringBackendLinear(prompt)
}

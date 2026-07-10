package initcmd

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/huh"

	"github.com/open-cli-collective/codereview-cli/internal/config"
)

func initCredentialStoreDefaultID() string {
	return config.LocalOSCredentialStoreID
}

func initCredentialStoreDraftValue(current string) string {
	current = strings.TrimSpace(current)
	if current == "" {
		return initCredentialStoreDefaultID()
	}
	return current
}

func initCredentialStoreOptions(cfg config.File) []huh.Option[string] {
	stores := config.EffectiveSecretsStores(cfg)
	options := make([]huh.Option[string], 0, len(stores))
	for _, store := range stores {
		options = append(options, huh.NewOption(initCredentialStoreOptionLabel(cfg, store.ID), store.ID))
	}
	return options
}

func initCredentialStoreOptionLabel(cfg config.File, storeID string) string {
	storeID = strings.TrimSpace(storeID)
	if storeID == config.LocalOSCredentialStoreID {
		description := strings.TrimSpace(initBuiltInOSCredentialStoreDescription())
		if description == "" {
			return initBuiltInOSCredentialStoreTitle()
		}
		return fmt.Sprintf("%s - %s", initBuiltInOSCredentialStoreTitle(), description)
	}
	store, ok := cfg.Secrets.Stores[storeID]
	if !ok {
		return storeID
	}
	name := strings.TrimSpace(store.DisplayName)
	if name == "" {
		name = storeID
	}
	parts := []string{name}
	if detail := initCredentialConfiguredStoreDetail(store); detail != "" {
		parts = append(parts, detail)
	} else if backend := initSecretsBackendDisplayLabel(store.Backend.Kind); backend != "" {
		parts = append(parts, backend)
	}
	return strings.Join(parts, " - ")
}

func initCredentialConfiguredStoreDetail(store config.SecretsStore) string {
	if store.Backend.OnePassword != nil && config.IsOnePasswordSecretsBackend(store.Backend.Kind) {
		var parts []string
		if vault := firstNonEmpty(store.Backend.OnePassword.VaultName, store.Backend.OnePassword.VaultID); vault != "" {
			parts = append(parts, vault+" vault")
		}
		if account := firstNonEmpty(store.Backend.OnePassword.AccountURL, store.Backend.OnePassword.AccountID); account != "" {
			parts = append(parts, account)
		}
		return strings.Join(parts, ", ")
	}
	return ""
}

func initCredentialStoreDisplayName(cfg config.File, storeID string) string {
	storeID = strings.TrimSpace(storeID)
	if storeID == "" || storeID == config.LocalOSCredentialStoreID {
		return initBuiltInOSCredentialStoreTitle()
	}
	if store, ok := cfg.Secrets.Stores[storeID]; ok {
		if displayName := strings.TrimSpace(store.DisplayName); displayName != "" {
			return displayName
		}
	}
	return storeID
}

func initCredentialDestinationLabel(cfg config.File, location config.CredentialLocation) string {
	location.Store = strings.TrimSpace(location.Store)
	location.Name = strings.TrimSpace(location.Name)
	if location.Store == "" {
		location.Store = initCredentialStoreDefaultID()
	}
	parts := []string{initCredentialStoreDisplayName(cfg, location.Store)}
	if store, ok := cfg.Secrets.Stores[location.Store]; ok {
		if store.Backend.OnePassword != nil && config.IsOnePasswordSecretsBackend(store.Backend.Kind) {
			if vault := firstNonEmpty(store.Backend.OnePassword.VaultName, store.Backend.OnePassword.VaultID); vault != "" {
				parts = append(parts, vault)
			}
		}
	}
	if location.Name != "" {
		parts = append(parts, location.Name)
	}
	return strings.Join(parts, " / ")
}

func initCredentialLocation(storeID, name string) config.CredentialLocation {
	storeID = strings.TrimSpace(storeID)
	if storeID == "" {
		storeID = initCredentialStoreDefaultID()
	}
	return config.CredentialLocation{
		Store: storeID,
		Name:  strings.TrimSpace(name),
	}
}

func initCredentialLocationIfName(storeID, name string) config.CredentialLocation {
	name = strings.TrimSpace(name)
	if name == "" {
		return config.CredentialLocation{}
	}
	return initCredentialLocation(storeID, name)
}

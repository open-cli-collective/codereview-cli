package configedit

import (
	"errors"
	"reflect"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/config"
)

var (
	// ErrSecretsStoreIDRequired means a credential-store id was omitted.
	ErrSecretsStoreIDRequired = errors.New("secrets-profile id is required")
	// ErrSecretsStoreReserved means the caller targeted a reserved projected id.
	ErrSecretsStoreReserved = errors.New("secrets-profile id is reserved")
	// ErrSecretsStoreBackendRequired means a create operation omitted the backend.
	ErrSecretsStoreBackendRequired = errors.New("secrets-profile backend is required")
	// ErrSecretsStoreMutationRequired means an update operation omitted mutation flags.
	ErrSecretsStoreMutationRequired = errors.New("secrets-profile mutation is required")
	// ErrSecretsStoreLabelConflict means both set and clear label were requested.
	ErrSecretsStoreLabelConflict = errors.New("secrets-profile label flags conflict")
	// ErrSecretsStoreLabelRequired means a provided label was blank after trim.
	ErrSecretsStoreLabelRequired = errors.New("secrets-profile label is required")
)

// SecretsStorePatch describes a config-only update to one named credential store.
type SecretsStorePatch struct {
	Backend    *config.SecretsStoreBackend
	Label      *string
	ClearLabel bool
}

// NormalizeSecretsStoreID trims and validates one credential-store id.
func NormalizeSecretsStoreID(raw string) (string, error) {
	id := strings.TrimSpace(raw)
	if id == "" {
		return "", ErrSecretsStoreIDRequired
	}
	if id == config.LocalOSCredentialStoreID {
		return "", ErrSecretsStoreReserved
	}
	return id, nil
}

// SetSecretsStore creates or updates one named credential store.
func SetSecretsStore(cfg config.File, rawID string, patch SecretsStorePatch) (config.File, bool, bool, error) {
	id, err := NormalizeSecretsStoreID(rawID)
	if err != nil {
		return config.File{}, false, false, err
	}
	if patch.Label != nil && patch.ClearLabel {
		return config.File{}, false, false, ErrSecretsStoreLabelConflict
	}
	var normalizedLabel *string
	if patch.Label != nil {
		trimmed := strings.TrimSpace(*patch.Label)
		if trimmed == "" {
			return config.File{}, false, false, ErrSecretsStoreLabelRequired
		}
		normalizedLabel = &trimmed
	}
	working := config.Normalize(cfg)
	working.Secrets.Stores = cloneSecretsStores(working.Secrets.Stores)
	if working.Secrets.Stores == nil {
		working.Secrets.Stores = map[string]config.SecretsStore{}
	}
	existing, existed := working.Secrets.Stores[id]
	if !existed && patch.Backend == nil {
		return config.File{}, false, false, ErrSecretsStoreBackendRequired
	}
	if existed && patch.Backend == nil && normalizedLabel == nil && !patch.ClearLabel {
		return config.File{}, false, false, ErrSecretsStoreMutationRequired
	}
	updated := existing
	if patch.Backend != nil {
		updated.Backend = *patch.Backend
	}
	if normalizedLabel != nil {
		updated.DisplayName = *normalizedLabel
	}
	if patch.ClearLabel {
		updated.DisplayName = ""
	}
	if !existed && strings.TrimSpace(string(updated.Backend.Kind)) == "" {
		return config.File{}, false, false, ErrSecretsStoreBackendRequired
	}
	if existed && reflect.DeepEqual(existing, updated) {
		return cfg, false, false, nil
	}
	working.Secrets.Stores[id] = updated
	if err := validateConfigAfterSecretsStoreEdit(working); err != nil {
		return config.File{}, false, false, err
	}
	return config.Normalize(working), true, !existed, nil
}

// RemoveSecretsStore removes one explicit named credential store.
func RemoveSecretsStore(cfg config.File, rawID string) (config.File, bool, error) {
	id, err := NormalizeSecretsStoreID(rawID)
	if err != nil {
		return config.File{}, false, err
	}
	working := config.Normalize(cfg)
	if _, ok := working.Secrets.Stores[id]; !ok {
		return cfg, false, nil
	}
	working.Secrets.Stores = cloneSecretsStores(working.Secrets.Stores)
	delete(working.Secrets.Stores, id)
	if len(working.Secrets.Stores) == 0 {
		working.Secrets.Stores = nil
	}
	if err := validateConfigAfterSecretsStoreEdit(working); err != nil {
		return config.File{}, false, err
	}
	return config.Normalize(working), true, nil
}

func validateConfigAfterSecretsStoreEdit(cfg config.File) error {
	cfg = config.Normalize(cfg)
	if len(cfg.Profiles) > 0 || len(cfg.RepositoryProfiles) > 0 {
		return config.Validate(cfg)
	}
	if err := config.ValidateSecrets(cfg.Secrets); err != nil {
		return err
	}
	return config.ValidateRetention(cfg.Data.Retention)
}

func cloneSecretsStores(in map[string]config.SecretsStore) map[string]config.SecretsStore {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]config.SecretsStore, len(in))
	for id, profile := range in {
		out[id] = profile
	}
	return out
}

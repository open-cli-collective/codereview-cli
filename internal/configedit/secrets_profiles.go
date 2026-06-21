package configedit

import (
	"errors"
	"reflect"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/config"
)

var (
	// ErrSecretsProfileIDRequired means a secrets-profile id was omitted.
	ErrSecretsProfileIDRequired = errors.New("secrets-profile id is required")
	// ErrSecretsProfileReserved means the caller targeted a reserved projected id.
	ErrSecretsProfileReserved = errors.New("secrets-profile id is reserved")
	// ErrSecretsProfileBackendRequired means a create operation omitted the backend.
	ErrSecretsProfileBackendRequired = errors.New("secrets-profile backend is required")
	// ErrSecretsProfileMutationRequired means an update operation omitted mutation flags.
	ErrSecretsProfileMutationRequired = errors.New("secrets-profile mutation is required")
	// ErrSecretsProfileLabelConflict means both set and clear label were requested.
	ErrSecretsProfileLabelConflict = errors.New("secrets-profile label flags conflict")
	// ErrSecretsProfileLabelRequired means a provided label was blank after trim.
	ErrSecretsProfileLabelRequired = errors.New("secrets-profile label is required")
)

// SecretsProfilePatch describes a config-only update to one named secrets-management profile.
type SecretsProfilePatch struct {
	Backend    *config.SecretsProfileBackend
	Label      *string
	ClearLabel bool
}

// NormalizeSecretsProfileID trims and validates one secrets-profile id.
func NormalizeSecretsProfileID(raw string) (string, error) {
	id := strings.TrimSpace(raw)
	if id == "" {
		return "", ErrSecretsProfileIDRequired
	}
	if id == config.LocalOSCredentialStoreID {
		return "", ErrSecretsProfileReserved
	}
	return id, nil
}

// SetSecretsProfile creates or updates one named secrets-management profile.
func SetSecretsProfile(cfg config.File, rawID string, patch SecretsProfilePatch) (config.File, bool, bool, error) {
	id, err := NormalizeSecretsProfileID(rawID)
	if err != nil {
		return config.File{}, false, false, err
	}
	if patch.Label != nil && patch.ClearLabel {
		return config.File{}, false, false, ErrSecretsProfileLabelConflict
	}
	var normalizedLabel *string
	if patch.Label != nil {
		trimmed := strings.TrimSpace(*patch.Label)
		if trimmed == "" {
			return config.File{}, false, false, ErrSecretsProfileLabelRequired
		}
		normalizedLabel = &trimmed
	}
	working := config.Normalize(cfg)
	working.Secrets.Profiles = cloneSecretsProfiles(working.Secrets.Profiles)
	working.Secrets.Stores = cloneSecretsProfiles(working.Secrets.Stores)
	if working.Secrets.Profiles == nil {
		working.Secrets.Profiles = map[string]config.SecretsProfile{}
	}
	if working.Secrets.Stores == nil {
		working.Secrets.Stores = map[string]config.SecretsStore{}
	}
	existing, existed := working.Secrets.Stores[id]
	if !existed && patch.Backend == nil {
		return config.File{}, false, false, ErrSecretsProfileBackendRequired
	}
	if existed && patch.Backend == nil && normalizedLabel == nil && !patch.ClearLabel {
		return config.File{}, false, false, ErrSecretsProfileMutationRequired
	}
	updated := existing
	if patch.Backend != nil {
		updated.Backend = *patch.Backend
	}
	if normalizedLabel != nil {
		updated.DisplayName = *normalizedLabel
		updated.Label = *normalizedLabel
	}
	if patch.ClearLabel {
		updated.DisplayName = ""
		updated.Label = ""
	}
	if !existed && strings.TrimSpace(string(updated.Backend.Kind)) == "" {
		return config.File{}, false, false, ErrSecretsProfileBackendRequired
	}
	if existed && reflect.DeepEqual(existing, updated) {
		return cfg, false, false, nil
	}
	working.Secrets.Profiles[id] = updated
	working.Secrets.Stores[id] = updated
	if err := validateConfigAfterSecretsProfileEdit(working); err != nil {
		return config.File{}, false, false, err
	}
	return config.Normalize(working), true, !existed, nil
}

// RemoveSecretsProfile removes one explicit named credential store.
func RemoveSecretsProfile(cfg config.File, rawID string) (config.File, bool, error) {
	id, err := NormalizeSecretsProfileID(rawID)
	if err != nil {
		return config.File{}, false, err
	}
	working := config.Normalize(cfg)
	if _, ok := working.Secrets.Stores[id]; !ok {
		return cfg, false, nil
	}
	working.Secrets.Profiles = cloneSecretsProfiles(working.Secrets.Profiles)
	working.Secrets.Stores = cloneSecretsProfiles(working.Secrets.Stores)
	delete(working.Secrets.Profiles, id)
	delete(working.Secrets.Stores, id)
	if len(working.Secrets.Profiles) == 0 {
		working.Secrets.Profiles = nil
	}
	if len(working.Secrets.Stores) == 0 {
		working.Secrets.Stores = nil
	}
	if err := validateConfigAfterSecretsProfileEdit(working); err != nil {
		return config.File{}, false, err
	}
	return config.Normalize(working), true, nil
}

func validateConfigAfterSecretsProfileEdit(cfg config.File) error {
	cfg = config.Normalize(cfg)
	if len(cfg.Profiles) > 0 || len(cfg.RepositoryProfiles) > 0 {
		return config.Validate(cfg)
	}
	if err := config.ValidateSecrets(cfg.Secrets); err != nil {
		return err
	}
	return config.ValidateRetention(cfg.Data.Retention)
}

func cloneSecretsProfiles(in map[string]config.SecretsProfile) map[string]config.SecretsProfile {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]config.SecretsProfile, len(in))
	for id, profile := range in {
		out[id] = profile
	}
	return out
}

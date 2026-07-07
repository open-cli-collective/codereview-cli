package credentials

import (
	"strings"

	"github.com/open-cli-collective/cli-common/credstore"

	"github.com/open-cli-collective/codereview-cli/internal/progress"
)

type progressReader struct {
	command string
	logger  *progress.Logger
	backend string
	base    Reader
}

// NewProgressReader wraps backend secret reads with structured progress spans.
func NewProgressReader(command string, logger *progress.Logger, resolved ResolvedSecretsProfile, base Reader) Reader {
	if base == nil || logger == nil {
		return base
	}
	return progressReader{
		command: progressCommand(command),
		logger:  logger,
		backend: resolved.Backend,
		base:    base,
	}
}

func (r progressReader) Get(profile, key string) (string, error) {
	span := startSecretSpan(r.logger, r.command, "read_secret", secretKeyTarget(r.backend, profile, key))
	value, err := r.base.Get(profile, key)
	endSecretSpan(span, err)
	return value, err
}

// ExistsWithProgress wraps a secret existence probe with structured progress.
func ExistsWithProgress(command string, logger *progress.Logger, store *credstore.Store, profile, key string) (bool, error) {
	span := startSecretSpan(logger, command, "probe_secret", secretKeyTarget(storeBackendName(store), profile, key))
	ok, err := store.Exists(profile, key)
	endSecretSpan(span, err)
	return ok, err
}

// ListBundleWithProgress wraps one secret-bundle listing with structured progress.
func ListBundleWithProgress(command string, logger *progress.Logger, store *credstore.Store, profile string) ([]string, error) {
	span := startSecretSpan(logger, command, "list_secret_bundle", secretBundleTarget(storeBackendName(store), profile))
	keys, err := store.ListBundle(profile)
	endSecretSpan(span, err)
	return keys, err
}

// SetWithProgress wraps one secret write with structured progress.
func SetWithProgress(command string, logger *progress.Logger, store *credstore.Store, profile, key, value string, opts ...credstore.SetOpt) error {
	span := startSecretSpan(logger, command, "write_secret", secretKeyTarget(storeBackendName(store), profile, key))
	err := store.Set(profile, key, value, opts...)
	endSecretSpan(span, err)
	return err
}

// SetBundleWithProgress wraps one secret-bundle write with structured progress.
func SetBundleWithProgress(command string, logger *progress.Logger, store *credstore.Store, profile string, kv map[string]string, opts ...credstore.SetOpt) (credstore.Result, error) {
	span := startSecretSpan(logger, command, "write_secret_bundle", secretBundleTarget(storeBackendName(store), profile))
	result, err := store.SetBundle(profile, kv, opts...)
	endSecretSpan(span, err)
	return result, err
}

// DeleteWithProgress wraps one secret deletion with structured progress.
func DeleteWithProgress(command string, logger *progress.Logger, store *credstore.Store, profile, key string) error {
	span := startSecretSpan(logger, command, "delete_secret", secretKeyTarget(storeBackendName(store), profile, key))
	err := store.Delete(profile, key)
	endSecretSpan(span, err)
	return err
}

// DeleteBundleWithProgress wraps one secret-bundle deletion with structured progress.
func DeleteBundleWithProgress(command string, logger *progress.Logger, store *credstore.Store, profile string) ([]string, error) {
	span := startSecretSpan(logger, command, "delete_secret_bundle", secretBundleTarget(storeBackendName(store), profile))
	keys, err := store.DeleteBundle(profile)
	endSecretSpan(span, err)
	return keys, err
}

func startSecretSpan(logger *progress.Logger, command, op, target string) *progress.Span {
	if logger == nil {
		return nil
	}
	return logger.Start(progressCommand(command), op, target)
}

func endSecretSpan(span *progress.Span, err error) {
	if span != nil {
		span.End(err)
	}
}

func progressCommand(command string) string {
	command = strings.TrimSpace(command)
	if command == "" {
		return "credentials"
	}
	return command
}

func storeBackendName(store *credstore.Store) string {
	if store == nil {
		return "credential-store"
	}
	backend, _ := store.Backend()
	if strings.TrimSpace(string(backend)) == "" {
		return "credential-store"
	}
	return string(backend)
}

func secretKeyTarget(backend, profile, key string) string {
	parts := []string{storeBackendLabel(backend), ServiceName}
	if value := strings.TrimSpace(profile); value != "" {
		parts = append(parts, value)
	}
	if value := strings.TrimSpace(key); value != "" {
		parts = append(parts, value)
	}
	return strings.Join(parts, "/")
}

func secretBundleTarget(backend, profile string) string {
	parts := []string{storeBackendLabel(backend), ServiceName}
	if value := strings.TrimSpace(profile); value != "" {
		parts = append(parts, value)
	}
	return strings.Join(parts, "/")
}

func storeBackendLabel(backend string) string {
	backend = strings.TrimSpace(backend)
	if backend == "" {
		return "credential-store"
	}
	return backend
}

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

// ProgressStoreReader adapts a credstore-backed reader and logs backend reads.
func ProgressStoreReader(command string, logger *progress.Logger, resolved ResolvedSecretsProfile, store *credstore.Store) Reader {
	if store == nil {
		return nil
	}
	if logger == nil {
		return NewStoreReader(store)
	}
	return progressReader{
		command: progressCommand(command),
		logger:  logger,
		backend: strings.TrimSpace(resolved.Backend),
		base:    NewStoreReader(store),
	}
}

func (r progressReader) Get(profile, key string) (string, error) {
	span := r.logger.Start(r.command, "read_secret_backend", secretTarget(r.backend, profile, key))
	value, err := r.base.Get(profile, key)
	span.End(err)
	return value, err
}

// ProgressCachingReader wraps a caching reader with cache hit/miss breadcrumbs.
func ProgressCachingReader(command string, logger *progress.Logger, storeID string, resolved ResolvedSecretsProfile, base Reader) CachedReader {
	if base == nil {
		return nil
	}
	reader := &cachingReader{
		storeID:  strings.TrimSpace(storeID),
		base:     base,
		logger:   logger,
		command:  progressCommand(command),
		backend:  strings.TrimSpace(resolved.Backend),
		cached:   map[readCacheKey]string{},
		inflight: map[readCacheKey]*inflightRead{},
	}
	return reader
}

func progressCommand(command string) string {
	command = strings.TrimSpace(command)
	if command == "" {
		return "credentials"
	}
	return command
}

func secretTarget(backend, profile, key string) string {
	backend = strings.TrimSpace(backend)
	if backend == "" {
		backend = "credential-store"
	}
	parts := []string{backend, ServiceName}
	if value := strings.TrimSpace(profile); value != "" {
		parts = append(parts, value)
	}
	if value := strings.TrimSpace(key); value != "" {
		parts = append(parts, value)
	}
	return strings.Join(parts, "/")
}

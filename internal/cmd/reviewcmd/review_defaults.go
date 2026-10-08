package reviewcmd

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/modelcatalog"
	"github.com/open-cli-collective/codereview-cli/internal/runlock"
)

func upgradeReviewDefaults(ctx context.Context, path, runtimeName string, snapshots ...*modelcatalog.Catalog) (config.File, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	lock, err := runlock.Acquire(path + ".review-defaults.lock")
	for errors.Is(err, runlock.ErrHeld) {
		select {
		case <-ctx.Done():
			return config.File{}, false, ctx.Err()
		case <-time.After(10 * time.Millisecond):
			lock, err = runlock.Acquire(path + ".review-defaults.lock")
		}
	}
	if err != nil {
		return config.File{}, false, err
	}
	defer func() { _ = lock.Release() }()
	// Concurrent reviews must re-read the marker and config under the same lock.
	var catalog *modelcatalog.Catalog
	if len(snapshots) > 0 {
		catalog = snapshots[0]
	} else {
		catalog, err = modelcatalog.LoadBundled()
		if err != nil {
			return config.File{}, false, err
		}
	}
	cfg, err := config.LoadWithCatalog(path, catalog)
	if err != nil {
		return config.File{}, false, err
	}
	upgraded, changed := config.UpgradeReviewDefaults(cfg, runtimeName)
	if !changed {
		return cfg, false, nil
	}
	if err := saveReviewDefaults(path, upgraded); errors.Is(err, config.ErrChanged) {
		// A normal config edit won the write lock. Re-read rather than overwrite it.
		_ = lock.Release()
		return upgradeReviewDefaults(ctx, path, runtimeName, catalog)
	} else if err != nil {
		return config.File{}, false, err
	}
	loaded, err := config.LoadWithCatalog(path, catalog)
	return loaded, err == nil, err
}

// Keep the original preferences before the one-time automatic upgrade. Config
// Save already stages and atomically renames the updated file.
func saveReviewDefaults(path string, cfg config.File) error {
	// #nosec G304 -- path is the resolved config path.
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// #nosec G304 -- backup stays beside the resolved config path.
	backup, err := os.OpenFile(path+".before-review-defaults-v1", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if err == nil {
		_, writeErr := backup.Write(body)
		closeErr := backup.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			// A partial backup must not become a successful backup on retry.
			_ = os.Remove(path + ".before-review-defaults-v1")
			return err
		}
	}
	return config.Save(path, cfg)
}

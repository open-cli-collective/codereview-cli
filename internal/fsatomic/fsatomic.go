// Package fsatomic provides atomic file writes and JSON file IO.
package fsatomic

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// WriteFileAtomic writes data to a sibling temporary file before replacing path.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// WriteJSON marshals v and writes it atomically with private permissions.
func WriteJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, append(data, '\n'), 0o600)
}

// ReadJSON reads path and unmarshals its JSON contents into v.
func ReadJSON(path string, v any) error {
	data, err := os.ReadFile(path) // #nosec G304 -- callers own the path.
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

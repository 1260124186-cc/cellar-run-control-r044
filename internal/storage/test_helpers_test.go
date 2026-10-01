package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
)

func statePath(dir string) string {
	return filepath.Join(dir, stateFileName)
}

func removeDirectory(dir string) error {
	return os.RemoveAll(dir)
}

// tamperSnapshotFile rewrites the persisted snapshot with an altered ledger
// payload while leaving the stored hash links untouched, simulating on-disk
// tampering that startup verification must detect.
func tamperSnapshotFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		return err
	}
	entries, ok := document["trajectory"].([]any)
	if !ok || len(entries) == 0 {
		return os.ErrInvalid
	}
	first, ok := entries[0].(map[string]any)
	if !ok {
		return os.ErrInvalid
	}
	first["payload"] = map[string]any{"tampered": true}
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, encoded, 0o644)
}

package arcade

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type deletionIntent struct {
	ID            string `json:"id"`
	Runtime       string `json:"runtime"`
	DeleteData    bool   `json:"delete_managed_data"`
	DeleteBackups bool   `json:"delete_backups"`
}

func (m *Manager) deletionPath(id string) string {
	return filepath.Join(m.dataDir, "deletions", id+".json")
}
func (m *Manager) beginDeletion(s *Server, data, backups bool) error {
	if err := os.MkdirAll(filepath.Join(m.dataDir, "deletions"), 0700); err != nil {
		return err
	}
	b, err := json.Marshal(deletionIntent{s.ID, s.Runtime, data, backups})
	if err != nil {
		return err
	}
	return writeDurable(m.deletionPath(s.ID), b)
}
func (m *Manager) completeDeletion(intent deletionIntent) error {
	if intent.Runtime == RuntimeDocker {
		switch containerState(intent.ID) {
		case ContainerStopped, ContainerMissing:
		default:
			return fmt.Errorf("server detached; data cleanup remains pending until Docker confirms its container is stopped or missing")
		}
	}
	// Direct panel path, never the adopted target. RemoveAll on a symlink
	// removes only the panel link, preserving all operator-owned external bytes.
	path := filepath.Join(m.dataDir, "servers", intent.ID)
	if intent.DeleteData {
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	} else if st, err := os.Lstat(path); err == nil && st.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	if intent.DeleteBackups {
		if err := os.RemoveAll(m.backupDir(intent.ID)); err != nil {
			return err
		}
	}
	return nil
}

// Tombstones suppress resurrection even if a crash preceded servers.json
// publication. Cleanup is retryable and only touches the named panel paths.
func (m *Manager) deletionIntents() (map[string]deletionIntent, error) {
	entries, err := os.ReadDir(filepath.Join(m.dataDir, "deletions"))
	if os.IsNotExist(err) {
		return map[string]deletionIntent{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]deletionIntent{}
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".arcade-tmp-journal-") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(m.dataDir, "deletions", entry.Name()))
		if err != nil {
			return nil, err
		}
		var intent deletionIntent
		if json.Unmarshal(b, &intent) != nil || intent.ID == "" || filepath.Base(intent.ID) != intent.ID || entry.Name() != intent.ID+".json" {
			return nil, fmt.Errorf("invalid deletion intent; recovery required")
		}
		out[intent.ID] = intent
	}
	return out, nil
}

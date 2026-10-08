package arcade

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
)

var errEditConflict = errors.New("this content changed since it was opened; reload or compare before saving")

func contentETag(content string) string {
	hash := sha256.Sum256([]byte(content))
	return `"` + hex.EncodeToString(hash[:]) + `"`
}
func settingsETag(s *Server) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.Marshal(struct {
		Props            map[string]string
		Port, MaxPlayers int
	}{s.Props, s.Port, s.MaxPlayers})
	return contentETag(string(b))
}
func (m *Manager) WriteFileConditional(s *Server, path, content, expected string) error {
	s.fsMu.RLock()
	defer s.fsMu.RUnlock()
	s.editMu.Lock()
	defer s.editMu.Unlock()
	if expected != "" {
		current, err := m.ReadFile(s, path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err != nil || contentETag(current) != expected {
			return errEditConflict
		}
	}
	return m.writeFileHeld(s, path, content)
}
func (m *Manager) ApplySettingsConditional(s *Server, changes map[string]string, expected string) ([]string, error) {
	s.fsMu.RLock()
	defer s.fsMu.RUnlock()
	s.editMu.Lock()
	defer s.editMu.Unlock()
	if expected != "" && settingsETag(s) != expected {
		return nil, errEditConflict
	}
	return m.applySettingsHeld(s, changes)
}
func editErrorCode(err error) int {
	if errors.Is(err, errEditConflict) {
		return 412
	}
	return 400
}

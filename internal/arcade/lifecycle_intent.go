package arcade

import "fmt"

func (m *Manager) persistIntent(s *Server, desired bool) (uint64, error) {
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	if err := m.requireRegistered(s); err != nil {
		return 0, err
	}
	s.mu.Lock()
	old, oldGeneration := s.DesiredRunning, s.IntentGeneration
	s.DesiredRunning = &desired
	s.IntentGeneration++
	generation := s.IntentGeneration
	s.mu.Unlock()
	if err := m.saveHeld(); err != nil {
		s.mu.Lock()
		s.DesiredRunning = old
		s.IntentGeneration = oldGeneration
		s.mu.Unlock()
		return 0, fmt.Errorf("could not persist lifecycle intent: %w", err)
	}
	return generation, nil
}

package httpapi

import "strings"

// Reserve the entire batch before changing any account. The same action keys
// fence quota renewal, quality checks and CPA delivery/removal, so a successful
// disable or deletion cannot race a credential upload already in progress.
func (s *Server) tryGrokOAuthAccountActions(ids []string) (func(), bool) {
	locked := make(map[string]bool, len(ids))
	unlock := func() {
		for id := range locked {
			s.grokOAuthActions.Delete(id)
		}
	}
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" || locked[id] {
			continue
		}
		if _, busy := s.grokOAuthActions.LoadOrStore(id, true); busy {
			unlock()
			return nil, false
		}
		locked[id] = true
	}
	return unlock, true
}

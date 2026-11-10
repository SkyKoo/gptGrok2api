package httpapi

import (
	"fmt"
	"github.com/auucoder/gptgrok2api-go/internal/model"
	"net/http"
)

func validateChannelOrder(channels, order []string) error {
	if len(channels) != len(order) {
		return fmt.Errorf("priority must contain every available channel exactly once")
	}
	seen := map[string]bool{}
	for _, c := range order {
		valid := false
		for _, x := range channels {
			if c == x {
				valid = true
			}
		}
		if !valid || seen[c] {
			return fmt.Errorf("invalid or duplicate channel")
		}
		seen[c] = true
	}
	return nil
}
func (s *Server) channelOrder(id string) ([]string, error) {
	defaults := model.MediaChannels(id)
	cfg, err := s.store.Config()
	if err != nil {
		return nil, err
	}
	raw, _ := cfg["model_channel_priorities"].(map[string]any)
	order := []string{}
	if list, ok := raw[id].([]any); ok {
		for _, v := range list {
			if c, ok := v.(string); ok {
				order = append(order, c)
			}
		}
	}
	// Newly qualified variants are appended, preserving administrators' order.
	filtered := []string{}
	seen := map[string]bool{}
	for _, c := range order {
		for _, d := range defaults {
			if c == d && !seen[c] {
				filtered = append(filtered, c)
				seen[c] = true
			}
		}
	}
	for _, c := range defaults {
		if !seen[c] {
			filtered = append(filtered, c)
		}
	}
	return filtered, nil
}
func (s *Server) modelRoutingAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	if r.Method == http.MethodPut {
		var body struct {
			Priorities map[string][]string `json:"priorities"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		values := map[string]any{}
		for id, order := range body.Priorities {
			channels := model.MediaChannels(id)
			if len(channels) < 2 {
				writeError(w, 400, "priority is only configurable for models with multiple channels", "invalid_request_error")
				return
			}
			if err := validateChannelOrder(channels, order); err != nil {
				writeError(w, 400, err.Error(), "invalid_request_error")
				return
			}
			values[id] = order
		}
		if _, err := s.store.UpdateConfig("model_channel_priorities", values); err != nil {
			writeError(w, 500, "could not save model priority", "storage_error")
			return
		}
	}
	rows := []map[string]any{}
	for _, m := range s.modelSpecs() {
		channels := model.MediaChannels(m.ID)
		if len(channels) == 0 {
			continue
		}
		order, err := s.channelOrder(m.ID)
		if err != nil {
			writeError(w, 503, "could not read model priority", "storage_error")
			return
		}
		rows = append(rows, map[string]any{"model": m.ID, "name": m.Name, "channels": channels, "priority": order})
	}
	writeJSON(w, 200, map[string]any{"models": rows})
}
func (s *Server) mediaCapabilities() map[string]model.MediaCapabilities {
	out := map[string]model.MediaCapabilities{}
	for _, m := range s.modelSpecs() {
		if len(model.MediaChannels(m.ID)) > 0 {
			out[m.ID] = model.MediaCapability(m.ID)
		}
	}
	return out
}

package httpapi

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/model"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

func (s *Server) imageConversationModel() (string, error) {
	cfg, err := s.store.Config()
	if err != nil {
		return "", err
	}
	routing, _ := cfg["openai_routing"].(map[string]any)
	value := strings.TrimSpace(stringValue(routing["image_conversation_model"]))
	if value == "" {
		value = "gpt-5-3"
	}
	return value, nil
}
func (s *Server) openAIRoutingAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	if r.Method == http.MethodPost {
		var body struct {
			ImageModel string `json:"image_conversation_model"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		body.ImageModel = strings.TrimSpace(body.ImageModel)
		m, ok := model.Find(s.modelSpecs(), body.ImageModel)
		if !ok || !m.Enabled || m.OwnedBy != "openai" || m.Capability&model.Chat == 0 {
			writeError(w, 400, "select a discovered ChatGPT conversation model or a verified compatibility model", "invalid_request_error")
			return
		}
		if _, err := s.store.UpdateConfig("openai_routing", map[string]any{"image_conversation_model": body.ImageModel}); err != nil {
			writeError(w, 500, "failed to save model routing", "server_error")
			return
		}
	}
	selected, err := s.imageConversationModel()
	if err != nil {
		writeError(w, 500, "failed to load model routing", "server_error")
		return
	}
	choices := []string{}
	for _, m := range s.modelSpecs() {
		if m.Enabled && m.OwnedBy == "openai" && m.Capability&model.Chat != 0 {
			choices = append(choices, m.ID)
		}
	}
	writeJSON(w, 200, map[string]any{"image_conversation_model": selected, "image_model_choices": choices, "text_model_policy": "client_requested", "cross_model_fallback": false})
}

func (s *Server) rejectDuringMaintenance(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return false
	}
	if !strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/v1/") {
		return false
	}
	// Operators must be able to pause/restore the existing survival scheduler.
	if r.URL.Path == "/api/register/openai/survival" {
		return false
	}
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "cfm-maintenance")); os.IsNotExist(err) {
		return false
	}
	w.Header().Set("Retry-After", "30")
	writeError(w, 503, "服务维护中，请稍后重试；已提交的任务继续执行", "server_error")
	return true
}

func (s *Server) routingFeedback(l *accounts.Lease, err error) {
	rejected := provider.IsModelRejected(err)
	if err == nil || rejected {
		_ = s.discovery.RecordOutcome(accounts.Identity(l.Account), l.Intent().Model, err == nil, rejected)
	}
	if !rejected {
		status := http.StatusOK
		if err != nil {
			status = upstreamStatus(err)
		}
		s.accountPool.FeedbackIntent(l, status, err)
	}
}
func (s *Server) routingRetry(l *accounts.Lease, err error, attempt int) bool {
	return s.quotaRetrySafe(l, err) && (s.shouldRetry(upstreamStatus(err), attempt) || (provider.IsModelRejected(err) && attempt < s.cfg.ChatMaxRetries))
}

type modelAttempt struct {
	OutputIndex   int      `json:"output_index"`
	Attempt       int      `json:"attempt"`
	SentModel     string   `json:"sent_model"`
	UpstreamModel string   `json:"upstream_model,omitempty"`
	QuotaFeatures []string `json:"quota_features"`
	Outcome       string   `json:"outcome"`
	Status        int      `json:"status,omitempty"`
	RetryReason   string   `json:"retry_reason,omitempty"`
}
type routingTrace struct {
	mu              sync.Mutex
	server          *Server
	request         *http.Request
	requested, sent string
	attempts        []modelAttempt
}

func (s *Server) newRoutingTrace(r *http.Request, requested, sent string) *routingTrace {
	trace := &routingTrace{server: s, request: r, requested: requested, sent: sent}
	trace.publishLocked()
	return trace
}
func (t *routingTrace) publishLocked() {
	snapshot := append([]modelAttempt(nil), t.attempts...)
	t.server.enrichRequestMonitor(t.request, map[string]any{"model_routing": map[string]any{"requested_model": t.requested, "sent_model": t.sent, "attempts": snapshot}})
}
func (t *routingTrace) begin(ctx context.Context, l *accounts.Lease, index, attempt int) (context.Context, func(error, bool)) {
	features := []string{}
	for k := range l.Intent().Costs() {
		features = append(features, k)
	}
	sort.Strings(features)
	t.mu.Lock()
	pos := len(t.attempts)
	t.attempts = append(t.attempts, modelAttempt{OutputIndex: index + 1, Attempt: attempt + 1, SentModel: t.sent, QuotaFeatures: features, Outcome: "running"})
	t.publishLocked()
	t.mu.Unlock()
	ctx = provider.WithModelObserver(ctx, func(slug string) {
		t.mu.Lock()
		defer t.mu.Unlock()
		t.attempts[pos].UpstreamModel = slug
		t.publishLocked()
	})
	return ctx, func(err error, retry bool) {
		t.mu.Lock()
		defer t.mu.Unlock()
		a := &t.attempts[pos]
		a.Outcome = "success"
		a.Status = 200
		if err != nil {
			a.Outcome = "failed"
			a.Status = upstreamStatus(err)
		}
		if retry {
			a.RetryReason = "same_model_account_retry"
		}
		t.publishLocked()
	}
}

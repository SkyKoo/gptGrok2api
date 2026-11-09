package httpapi

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
)

func (s *Server) refreshRegisteredGrokRuntime(w http.ResponseWriter, r *http.Request) {
	ids, ok := decodeIDs(w, r)
	if !ok {
		return
	}
	items, err := s.registerStore.GetAccounts(ids)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	byID := map[string]map[string]any{}
	for _, item := range items {
		byID[stringValue(item["id"])] = item
	}
	type result struct {
		id   string
		item map[string]any
	}
	results := make(chan result, len(ids))
	sem := make(chan struct{}, 10)
	var wg sync.WaitGroup
	for _, id := range ids {
		id := id
		wg.Add(1)
		go func() {
			defer wg.Done()
			item, found := byID[id]
			if !found {
				results <- result{id: id, item: map[string]any{"id": id, "ok": false, "refresh_status": "failed", "error": "本地账号不存在"}}
				return
			}
			token := strings.TrimSpace(stringValue(item["sso"]))
			if token == "" {
				results <- result{id: id, item: map[string]any{"id": id, "ok": false, "refresh_status": "failed", "error": "账号未保存 SSO 登录态"}}
				return
			}
			sem <- struct{}{}
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
			defer cancel()
			quotas, media, refreshErr := s.refreshGrokAccountQuotas(ctx, token, item)
			if refreshErr != nil {
				s.recordGrokCredentialFailure(token, refreshErr)
				_, _ = s.registerStore.UpdateRuntime(id, nil, nil, "", safeRefreshError(refreshErr))
				results <- result{id: id, item: map[string]any{"id": id, "ok": false, "refresh_status": "failed", "error": safeRefreshError(refreshErr), "media_quota": media}}
				return
			}
			results <- result{id: id, item: map[string]any{"id": id, "ok": true, "refresh_status": "success", "error": "", "quota": quotaMap(quotas), "media_quota": media}}
		}()
	}
	wg.Wait()
	close(results)
	ordered := make(map[string]map[string]any, len(ids))
	for item := range results {
		ordered[item.id] = item.item
	}
	responseItems := make([]map[string]any, 0, len(ids))
	okCount := 0
	for _, id := range ids {
		item := ordered[id]
		responseItems = append(responseItems, item)
		if item["ok"] == true {
			okCount++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"summary": map[string]int{"total": len(ids), "ok": okCount, "fail": len(ids) - okCount},
		"results": responseItems,
	})
}

func (s *Server) verifyRegisteredGrokRuntime(w http.ResponseWriter, r *http.Request) {
	ids, ok := decodeIDs(w, r)
	if !ok {
		return
	}
	items, err := s.registerStore.GetAccounts(ids)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	byID := map[string]map[string]any{}
	for _, item := range items {
		byID[stringValue(item["id"])] = item
	}
	results := make([]map[string]any, 0, len(ids))
	valid, invalid, unknown := 0, 0, 0
	for _, id := range ids {
		item, found := byID[id]
		if !found {
			results = append(results, map[string]any{"id": id, "status": "invalid", "error": "本地账号不存在"})
			invalid++
			continue
		}
		token := strings.TrimSpace(stringValue(item["sso"]))
		if token == "" {
			results = append(results, map[string]any{"id": id, "status": "invalid", "error": "账号未保存 SSO 登录态"})
			invalid++
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		quota, status, probeErr := s.verifyGrokCredential(ctx, token, item)
		cancel()
		if probeErr != nil {
			if status == "invalid" {
				invalid++
			} else {
				unknown++
			}
			results = append(results, map[string]any{"id": id, "status": status, "error": safeRefreshError(probeErr)})
			continue
		}
		valid++
		results = append(results, map[string]any{"id": id, "status": "valid", "quota": map[string]any{"remaining": quota.Remaining, "total": quota.Total}})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"summary": map[string]int{"total": len(results), "valid": valid, "invalid": invalid, "unknown": unknown},
		"results": results,
	})
}

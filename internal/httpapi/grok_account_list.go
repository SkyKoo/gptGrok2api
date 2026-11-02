package httpapi

import (
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
)

func grokListRuntimeStatus(row map[string]any) string {
	if !boolValue(row["enabled"], true) || stringValue(row["status"]) == "disabled" {
		return "disabled"
	}
	if accounts.IsAbnormal(row) {
		return "invalid"
	}
	status := strings.ToLower(stringValue(row["status"]))
	if status == "invalid" || status == "expired" {
		return status
	}
	if until, err := time.Parse(time.RFC3339Nano, stringValue(row["cooldown_until"])); err == nil && until.After(time.Now()) {
		return "cooling"
	}
	if status == "cooling" || status == "rate_limited" {
		return status
	}
	return "active"
}

// Runtime membership and usage come from the actual request pool, not the last
// quota refresh in the archive. The join never copies credentials into a row.
func (s *Server) attachGrokAccountRuntime(items []map[string]any, base map[string]int) (map[string]any, error) {
	rows, _, err := s.grokRuntimeStore.AccountSnapshot()
	if err != nil {
		return nil, err
	}
	archive, err := s.registerStore.ExportAccounts(nil)
	if err != nil {
		return nil, err
	}
	credentials, err := s.oauthStore.PublicList()
	if err != nil {
		return nil, err
	}
	runtime := map[string]map[string]any{}
	for _, row := range rows {
		if token := accountToken(row); token != "" {
			runtime[token] = row
		}
	}
	byID := map[string]map[string]any{}
	for _, row := range archive {
		if token := stringValue(row["sso"]); token != "" {
			byID[stringValue(row["id"])] = runtime[token]
		}
	}
	byEmail := map[string]map[string]any{}
	for _, row := range credentials {
		if email := strings.ToLower(strings.TrimSpace(stringValue(row["email"]))); email != "" {
			byEmail[email] = row
		}
	}
	summary := map[string]any{}
	for key, value := range base {
		summary[key] = value
	}
	synced, linked, calls := 0, 0, 0
	runtimeStatus := map[string]int{"active": 0, "cooling": 0, "invalid": 0, "disabled": 0}
	for _, row := range rows {
		runtimeStatus[grokListRuntimeStatus(row)]++
		calls += intValue(row["use_count"]) + intValue(row["fail_count"])
	}
	for _, item := range items {
		if active := byID[stringValue(item["id"])]; active != nil {
			synced++
			item["sync_state"] = "synced"
			item["runtime_status"] = grokListRuntimeStatus(active)
			for _, key := range []string{"pool", "use_count", "fail_count", "last_used_at", "usage_started_at", "tags", "media_quota"} {
				if value, ok := active[key]; ok {
					item[key] = value
				}
			}
			item["media_quota_refresh_needed"] = active["media_quota_refresh_needed"] == true
			pending := mapValue(active["quota_pending"])
			item["media_quota_pending"] = map[string]int{"image_gen": intValue(pending["image_gen"]), "video_gen": intValue(pending["video_gen"])}
		} else {
			delete(item, "runtime_status")
			item["sync_state"] = "not_synced"
			if item["has_sso"] != true {
				item["sync_state"] = "not_ready"
			}
		}
		if credential := byEmail[strings.ToLower(strings.TrimSpace(stringValue(item["email"])))]; credential != nil {
			linked++
			item["oauth"] = credential
		}
	}
	summary["synced"], summary["not_synced"] = synced, len(items)-synced
	summary["runtime_total"], summary["runtime_status"], summary["calls_total"] = len(rows), runtimeStatus, calls
	summary["oauth_total"], summary["oauth_linked"] = len(credentials), linked
	return summary, nil
}

func grokAccountListMatches(item map[string]any, keyword, status string) bool {
	if keyword = strings.ToLower(strings.TrimSpace(keyword)); keyword != "" {
		text := strings.ToLower(strings.Join([]string{stringValue(item["id"]), stringValue(item["email"]), stringValue(item["source_type"]), stringValue(item["status"])}, " "))
		if !strings.Contains(text, keyword) {
			return false
		}
	}
	switch status {
	case "", "all":
		return true
	case "normal":
		return item["runtime_status"] == "active"
	case "limited":
		return item["runtime_status"] == "cooling" || item["runtime_status"] == "rate_limited"
	case "abnormal":
		return item["runtime_status"] == "invalid" || item["runtime_status"] == "expired"
	case "disabled":
		return item["runtime_status"] == "disabled"
	case "refresh_failed":
		return item["refresh_status"] == "failed"
	case "probe_invalid":
		return item["probe_status"] == "invalid"
	case "probe_unknown":
		return item["probe_status"] == "unknown"
	}
	if strings.HasPrefix(status, "oauth_") {
		credential := mapValue(item["oauth"])
		state := "unauthorized"
		if len(credential) > 0 {
			state = "unknown"
			probe := mapValue(credential["probe"])
			code := strings.ToLower(stringValue(probe["code"]))
			switch {
			case code == "personal-team-blocked" || strings.HasPrefix(code, "personal-team-blocked:"):
				state = "denied"
			case code == "insufficient_quota" || code == "usage_limit_reached":
				state = "no_quota"
			case credential["status"] == "disabled" || credential["status"] == "invalid":
				state = "invalid"
			case credential["status"] == "expired" || intValue(credential["expires_at"]) > 0 && int64(intValue(credential["expires_at"])) <= time.Now().Unix():
				state = "expired"
			default:
				switch stringValue(probe["status"]) {
				case "valid":
					state = "normal"
				case "limited":
					state = "limited"
				case "denied":
					state = "denied"
				case "invalid":
					state = "invalid"
				}
			}
		} else if mapValue(item["oauth_authorization"])["status"] == "denied" {
			state = "denied"
		}
		return status == "oauth_"+state
	}
	return strings.EqualFold(stringValue(item["status"]), status)
}

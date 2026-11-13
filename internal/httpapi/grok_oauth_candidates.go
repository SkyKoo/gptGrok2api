package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/oauth"
)

// Candidates project the Grok account inventory, including accounts with no
// OAuth credential yet. Never send SSO, cookies or tokens to this list.
func (s *Server) grokOAuthCandidates(w http.ResponseWriter, r *http.Request) {
	items, err := s.registerStore.ExportAccounts(nil)
	if err != nil {
		writeError(w, 500, "无法读取 Grok 账号", "server_error")
		return
	}
	credentials, err := s.oauthStore.List()
	if err != nil {
		writeError(w, 500, "无法读取 xAI OAuth 状态", "server_error")
		return
	}
	runtime, err := s.grokRuntimeStore.AccountList()
	if err != nil {
		writeError(w, 500, "无法读取 Grok 登录状态", "server_error")
		return
	}
	byToken := map[string]string{}
	for _, row := range runtime {
		byToken[accountToken(row)] = stringValue(row["status"])
	}
	byEmail := map[string]oauth.Account{}
	for _, credential := range credentials {
		if email := strings.ToLower(strings.TrimSpace(credential.Email)); email != "" {
			byEmail[email] = credential
		}
	}
	activeJobs := map[string]map[string]any{}
	for _, task := range s.taskQueue.List() {
		if task.Kind == "grok_oauth_authorize" && (task.Status == "queued" || task.Status == "running") {
			activeJobs[stringValue(task.Payload["account_id"])] = grokOAuthPublicJob(task)
		}
	}
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		email := strings.TrimSpace(stringValue(item["email"]))
		credential, matched := byEmail[strings.ToLower(email)]
		state := "pending"
		if matched && (credential.AccessToken != "" || credential.RefreshToken != "") {
			state = "authorized"
			if !oauth.CPAManaged(credential) && (credential.Status == "invalid" || credential.Status == "expired" || credential.Status == "disabled" || credential.ExpiresAt > 0 && credential.ExpiresAt <= time.Now().Unix()) {
				state = "requires_renewal"
			}
		} else if stringValue(mapValue(item["oauth_authorization"])["status"]) == "failed" {
			state = "failed"
		}
		authorization := mapValue(item["oauth_authorization"])
		jobID, message := "", ""
		if job := activeJobs[stringValue(item["id"])]; job != nil {
			state = stringValue(job["status"])
			if state == "pending" {
				state = "queued"
			}
			jobID = stringValue(job["id"])
		} else if stringValue(authorization["status"]) == "manual_action_required" {
			state = "manual_action_required"
			message = grokOAuthMessages[grokOAuthErrorCode(errors.New(stringValue(authorization["error"])))]
		} else if stringValue(authorization["status"]) == "failed" {
			state = "failed"
			message = grokOAuthMessages[grokOAuthErrorCode(errors.New(stringValue(authorization["error"])))]
		}
		loginStatus := stringValue(item["status"])
		if value := byToken[strings.TrimSpace(stringValue(item["sso"]))]; value != "" {
			loginStatus = value
		}
		result = append(result, map[string]any{
			"id": stringValue(item["id"]), "email": email, "login_status": loginStatus,
			"oauth_status": state, "oauth_account_id": credential.ID,
			"oauth_job_id": jobID, "oauth_message": message,
			"cpa_sync": credential.CPASync, "cpa_managed": oauth.CPAManaged(credential),
			"quality":          credential.Quality,
			"oauth":            oauth.Public(credential),
			"has_access_token": credential.AccessToken != "", "has_refresh_token": credential.RefreshToken != "",
			"updated_at": item["updated_at"],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": result, "total": len(result)})
}

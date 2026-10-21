package httpapi

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/store"
)

// Browser session JSON is untrusted input; only import identity and the web AT.
// Passwords, refresh tokens, browser cookies and status fields are not accepted.
func sessionImportPayload(session map[string]any) (map[string]any, error) {
	text := func(value any) string { s, _ := value.(string); return strings.TrimSpace(s) }
	first := func(a, b any) string {
		if s := text(a); s != "" {
			return s
		}
		return text(b)
	}
	token := first(session["accessToken"], session["access_token"])
	if token == "" {
		return nil, errors.New("Session JSON 中没有找到 accessToken")
	}
	user := mapValue(session["user"])
	for _, field := range []struct{ top, nested string }{{"email", "email"}, {"user_id", "id"}} {
		a, b := text(session[field.top]), text(user[field.nested])
		equal := a == b
		if field.top == "email" {
			equal = strings.EqualFold(a, b)
		}
		if a != "" && b != "" && !equal {
			return nil, errors.New("Session JSON 账号身份字段不一致")
		}
	}
	payload := map[string]any{"access_token": token, "source_type": "session_json", "status": "异常", "enabled": true, "quota_refresh_needed": true, "last_refresh_error": "待校验账号信息"}
	for key, value := range map[string]string{"email": first(session["email"], user["email"]), "user_id": first(session["user_id"], user["id"]), "expired": first(session["expires"], session["expired"])} {
		if value != "" {
			payload[key] = value
		}
	}
	return payload, nil
}

func (s *Server) accountSessionImport(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		Session map[string]any `json:"session"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "cfm-maintenance")); !os.IsNotExist(err) {
		writeError(w, http.StatusConflict, "服务维护中，请稍后导入", "invalid_request_error")
		return
	}
	payload, err := sessionImportPayload(body.Session)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	account, added, err := s.store.ImportSessionAccount(payload)
	if errors.Is(err, store.ErrSessionIdentityConflict) {
		writeError(w, http.StatusConflict, "Session JSON 与已保存账号身份不一致，未修改原账号", "identity_conflict")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "保存 Session 账号失败", "server_error")
		return
	}
	token := accountToken(account)
	// The total deadline stays below the console's 60 second request timeout.
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	candidate := cloneMap(account)
	// Require full identity and quota snapshots before declaring import verified.
	candidate["relogin_blocked"] = true
	candidate["source_type"] = "chatgpt_web"
	delete(candidate, "refresh_token")
	delete(candidate, "id_token")
	unchanged := func(current map[string]any) bool {
		return stringValue(current["email"]) == stringValue(account["email"]) && stringValue(current["user_id"]) == stringValue(account["user_id"])
	}
	result, refreshErr := s.refreshValidatedAccount(ctx, candidate)
	refreshed := 0
	if refreshErr == nil {
		account, _, err = s.store.RotateAccountTokensIf(token, result.AccessToken, "", "", result.Fields, unchanged)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "账号已保存，但校验结果保存失败，请刷新列表检查", "server_error")
			return
		}
		refreshed = 1
	} else {
		diagnostic := validationDiagnostic(refreshErr)
		updates := accountRefreshFailureUpdates(diagnostic)
		if updates == nil {
			updates = map[string]any{}
		}
		updates["quota_refresh_needed"] = true
		updates["last_refresh_error"] = validationDiagnostic(refreshErr).Error()
		updates["last_refresh_error_at"] = time.Now().UTC().Format(time.RFC3339)
		saved, _, saveErr := s.store.UpdateAccountIf(token, updates, unchanged)
		if saveErr != nil {
			writeError(w, http.StatusInternalServerError, "账号已保存，但校验状态保存失败，请刷新列表检查", "server_error")
			return
		}
		account = saved
	}
	response := map[string]any{"added": 0, "updated": 0, "refreshed": refreshed, "account_ref": accountPublicRef(account), "email": stringValue(account["email"]), "refresh_error": ""}
	if added {
		response["added"] = 1
	} else {
		response["updated"] = 1
	}
	if refreshErr != nil {
		response["refresh_error"] = validationDiagnostic(refreshErr).Error()
	}
	writeJSON(w, http.StatusOK, response)
}

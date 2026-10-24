package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeleteAccountSynchronizesHMEMailbox(t *testing.T) {
	loginCalls, listCalls, deleteCalls := 0, 0, 0
	hme := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respond := func(data any) { _ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data}) }
		switch r.URL.Path {
		case "/api/auth/login":
			loginCalls++
			http.SetCookie(w, &http.Cookie{Name: "hme_session", Value: "session", Path: "/"})
			respond(map[string]string{"csrf_token": "csrf"})
		case "/api/mailboxes":
			listCalls++
			respond(map[string]any{"mailboxes": []map[string]string{{
				"id": "hme-mailbox-1", "account_id": "hme-account", "email": "alias@example.test",
			}}})
		case "/api/mailboxes/hme-mailbox-1":
			deleteCalls++
			respond(map[string]any{"success": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer hme.Close()

	s := New(adminTestConfig(t.TempDir()))
	_, err := s.registerStore.Update(map[string]any{
		"target": "openai",
		"mail": map[string]any{"providers": []any{map[string]any{
			"type": "icloud_hme", "enable": true, "api_base": hme.URL,
			"admin_password": "private-password", "account_id": "hme-account",
			"delete_hme_mailbox_on_account_delete": true,
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.store.AddAccounts(nil, []map[string]any{{
		"access_token": "cfm-token",
		"email":        "alias@example.test",
		"registration_mailbox": map[string]any{
			"provider": "icloud_hme", "account_id": "hme-account", "email": "alias@example.test",
		},
	}}); err != nil {
		t.Fatal(err)
	}
	response := adminRequest(s.Handler(), http.MethodDelete, "/api/accounts", strings.NewReader(`{"tokens":["cfm-token"]}`))
	if response.Code != http.StatusOK {
		t.Fatalf("delete returned %d: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, `"deleted":1`) || strings.Contains(body, `"failed":1`) || strings.Contains(body, "private-password") {
		t.Fatalf("unexpected cleanup result: %s", body)
	}
	if loginCalls != 1 || listCalls != 1 || deleteCalls != 1 {
		t.Fatalf("unexpected HME calls login=%d list=%d delete=%d", loginCalls, listCalls, deleteCalls)
	}
	items, _ := s.store.AccountList()
	if len(items) != 0 {
		t.Fatalf("CFM account was not removed: %#v", items)
	}
}
func TestDeleteAccountDoesNotCallHMEWhenDisabled(t *testing.T) {
	calls := 0
	hme := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer hme.Close()
	s := New(adminTestConfig(t.TempDir()))
	_, err := s.registerStore.Update(map[string]any{
		"target": "openai",
		"mail": map[string]any{"providers": []any{map[string]any{
			"type": "icloud_hme", "enable": true, "api_base": hme.URL,
			"admin_password": "private-password", "account_id": "hme-account",
			"delete_hme_mailbox_on_account_delete": false,
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.store.AddAccounts(nil, []map[string]any{{
		"access_token": "cfm-token",
		"registration_mailbox": map[string]any{
			"provider": "icloud_hme", "account_id": "hme-account", "email": "alias@example.test",
		},
	}}); err != nil {
		t.Fatal(err)
	}
	response := adminRequest(s.Handler(), http.MethodDelete, "/api/accounts", strings.NewReader(`{"tokens":["cfm-token"]}`))
	if response.Code != http.StatusOK || calls != 0 {
		t.Fatalf("disabled cleanup unexpectedly called HME: code=%d calls=%d body=%s", response.Code, calls, response.Body.String())
	}
}

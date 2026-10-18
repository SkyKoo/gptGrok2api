package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublicCodexCredentialRedactsTokens(t *testing.T) {
	public := publicCodexCredential(map[string]any{
		"access_token":    "access-secret",
		"refresh_token":   "refresh-secret",
		"id_token":        "id-secret",
		"email":           "person@example.test",
		"status":          "authorized",
		"cpa_sync_status": "synced",
	})
	if _, ok := public["access_token"]; ok {
		t.Fatal("access token leaked")
	}
	if public["email"] != "person@example.test" || public["status"] != "authorized" {
		t.Fatalf("public credential lost safe fields: %#v", public)
	}
	for _, key := range []string{"has_access_token", "has_refresh_token", "has_id_token"} {
		if public[key] != true {
			t.Fatalf("missing %s marker: %#v", key, public)
		}
	}
}

func TestSafeOAuthErrorRedactsCredentialValues(t *testing.T) {
	err := safeOAuthError(fmt.Errorf(`{"access_token":"access-secret","refresh_token":"refresh-secret"} eyJabcdefghijklmnopqrstuvwxyz123456.abcdefghijklmnopqrstuvwxyz123456.abcdefghijklmnopqrstuvwxyz123456`))
	if strings.Contains(err, "access-secret") || strings.Contains(err, "refresh-secret") || strings.Contains(err, "eyJabcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("credential leaked in error: %s", err)
	}
}

func TestUploadCodexAuthFileUsesCPAAuthFilesMultipart(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v0/management/auth-files" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer cpa-secret" || r.Header.Get("X-Management-Key") != "cpa-secret" {
			t.Fatalf("missing CPA credentials: %#v", r.Header)
		}
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
			t.Fatalf("unexpected content type: %s", r.Header.Get("Content-Type"))
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if header.Filename != "codex-person.json" {
			t.Fatalf("unexpected filename: %q", header.Filename)
		}
		raw, err := io.ReadAll(file)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]string
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["type"] != "codex" || payload["access_token"] != "access-secret" || payload["refresh_token"] != "refresh-secret" {
			t.Fatalf("unexpected CPA payload: %#v", payload)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	result, err := uploadCodexAuthFile(context.Background(), externalCPAPool{BaseURL: server.URL, SecretKey: "cpa-secret"}, "codex-person.json", map[string]string{
		"type": "codex", "access_token": "access-secret", "refresh_token": "refresh-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := result["ok"].(bool); !ok || !value {
		t.Fatalf("unexpected CPA response: %#v", result)
	}
}

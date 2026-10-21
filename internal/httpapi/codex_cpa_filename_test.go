package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func codexFilenameJWT(accountID, plan string) string {
	payload, _ := json.Marshal(map[string]any{"sub": "user-not-a-workspace", "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": accountID, "chatgpt_plan_type": plan}})
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".fixture"
}

func TestCodexCPAFileNameMatchesCPAAccountHashEmailAndPlan(t *testing.T) {
	account := map[string]any{"id": "cfm-local-id", "email": "person@example.test", "type": "free"}
	cases := []struct {
		name       string
		credential map[string]any
		want       string
	}{
		{"id token identity and plan", map[string]any{"id_token": codexFilenameJWT("workspace-one", "Pro"), "access_token": codexFilenameJWT("workspace-two", "free"), "account_id": "stored-old-id", "plan_type": "plus"}, "codex-df52114f-person@example.test-pro.json"},
		{"access token fallback", map[string]any{"id_token": "invalid", "access_token": codexFilenameJWT("workspace-one", "Team Plan")}, "codex-df52114f-person@example.test-team-plan.json"},
		{"same email different account", map[string]any{"id_token": codexFilenameJWT("workspace-two", "free")}, "codex-831ad5d8-person@example.test-free.json"},
		{"saved identity fallback", map[string]any{"account_id": "workspace-one", "plan_type": "plus", "email": "alias+tag@example.test"}, "codex-df52114f-alias+tag@example.test-plus.json"},
		{"missing plan defaults free", map[string]any{"id_token": codexFilenameJWT("workspace-one", "")}, "codex-df52114f-person@example.test-free.json"},
		{"sub and CFM id are not account hash", map[string]any{"id_token": codexFilenameJWT("", "k12"), "account_id": "user-not-a-workspace"}, "codex-person@example.test-k12.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := codexCPASyncFileName(account, tc.credential, "destination")
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestCodexCPAFileNameKeepsExistingAndRejectsUnsafeNames(t *testing.T) {
	account := map[string]any{"email": "person@example.test"}
	for _, tc := range []struct {
		name       string
		credential map[string]any
		want       string
	}{
		{"source", map[string]any{"cpa_source_pool_id": "pool", "cpa_source_file_name": "turb-original.json", "cpa_pool_id": "pool", "cpa_file_name": "other.json"}, "turb-original.json"},
		{"legacy sync", map[string]any{"cpa_pool_id": "pool", "cpa_file_name": "codex-person-example.test.json", "id_token": codexFilenameJWT("workspace-one", "pro")}, "codex-person-example.test.json"},
		{"different destination", map[string]any{"cpa_source_pool_id": "other-pool", "cpa_source_file_name": "original.json", "cpa_pool_id": "other-pool", "cpa_file_name": "previous.json", "account_id": "workspace-one"}, "codex-df52114f-person@example.test-free.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := codexCPASyncFileName(account, tc.credential, "pool")
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v", got, err)
			}
		})
	}
	for _, credential := range []map[string]any{
		{"email": "../person@example.test"}, {"email": "person\n@example.test"}, {"email": strings.Repeat("x", 250) + "@example.test"},
		{"cpa_source_pool_id": "pool"}, {"cpa_source_pool_id": "pool", "cpa_source_file_name": "../private.json"},
		{"cpa_pool_id": "pool", "cpa_file_name": "nested/file.json"},
	} {
		if _, err := codexCPASyncFileName(account, credential, "pool"); err == nil {
			t.Fatal("unsafe or missing source filename accepted")
		}
	}
}

func TestCodexCPASyncUsesStandardNameAndReusesItAfterPlanChange(t *testing.T) {
	f := newCPABindingFixture(t)
	credential := map[string]any{"access_token": "fixture-new-access", "refresh_token": "fixture-new-refresh", "id_token": codexFilenameJWT("workspace-one", "plus"), "email": "person@example.test", "account_id": "workspace-one", "status": "authorized"}
	f.s.store.UpdateAccount("fixture.chatgpt.session", map[string]any{"codex_credential": credential})
	code, _ := f.call(t, "POST", "/api/accounts/codex/sync", map[string]any{"account_ref": "cfm-one", "cpa_pool_id": "fixture-pool"}, "fixture-admin")
	want := "codex-df52114f-person@example.test-plus.json"
	if code != 200 || len(f.uploaded) != 1 || f.uploaded[0] != want {
		t.Fatalf("sync did not use CPA convention: HTTP %d, files %v", code, f.uploaded)
	}
	payload := f.files[want]
	if payload["plan_type"] != "plus" || payload["email"] != "person@example.test" || payload["refresh_token"] != "fixture-new-refresh" {
		t.Fatal("sync metadata or credential changed")
	}
	account, _ := f.s.accountByRef("cfm-one")
	updated := mapValue(account["codex_credential"])
	updated["id_token"] = codexFilenameJWT("workspace-one", "pro")
	f.s.store.UpdateAccount("fixture.chatgpt.session", map[string]any{"codex_credential": updated})
	code, _ = f.call(t, "POST", "/api/accounts/codex/sync", map[string]any{"account_ref": "cfm-one", "cpa_pool_id": "fixture-pool"}, "fixture-admin")
	if code != 200 || len(f.uploaded) != 2 || f.uploaded[1] != want || f.files[want]["plan_type"] != "pro" {
		t.Fatal("repeat sync created duplicate instead of updating metadata")
	}
}

func TestCodexCPAPlanMetadataSurvivesImport(t *testing.T) {
	payload := map[string]any{"type": "codex", "access_token": "opaque-demo", "refresh_token": "demo-refresh", "email": "person@example.test", "account_id": "workspace-one", "plan_type": "team"}
	parsed, err := parseCPACredential(payload)
	if err != nil || parsed["plan_type"] != "team" {
		t.Fatal("import dropped saved plan metadata")
	}
	credential := codexCredential(map[string]string{"access_token": "opaque-demo", "id_token": codexFilenameJWT("workspace-one", "pro")}, "person@example.test", "workspace-one")
	if credential["plan_type"] != "pro" {
		t.Fatal("OAuth dropped token plan metadata")
	}
}

func TestCodexCPALegacySyncMigratesOnlyAfterRemoteDeletion(t *testing.T) {
	const oldName = "codex-person-example.test.json"
	const newName = "codex-df52114f-person@example.test-plus.json"
	for _, tc := range []struct {
		name                           string
		oldExists, newExists, imported bool
		want                           string
		wantDownloads                  int
	}{
		{"old still exists", true, false, false, oldName, 1},
		{"old deleted", false, false, false, newName, 2},
		{"new name occupied", false, true, false, "", 2},
		{"imported old missing", false, false, true, oldName, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCPABindingFixture(t)
			credential := map[string]any{"access_token": "fixture-new-access", "refresh_token": "fixture-new-refresh", "id_token": codexFilenameJWT("workspace-one", "plus"), "email": "person@example.test", "account_id": "workspace-one", "status": "authorized", "cpa_pool_id": "fixture-pool", "cpa_file_name": oldName, "cpa_sync_status": "synced", "cpa_synced_at": "2026-10-20T00:00:00Z"}
			if tc.imported {
				credential["cpa_source_pool_id"], credential["cpa_source_file_name"] = "fixture-pool", oldName
			}
			if tc.oldExists {
				f.files[oldName] = map[string]any{"type": "codex"}
			}
			if tc.newExists {
				f.files[newName] = map[string]any{"refresh_token": "remote-refreshed-token"}
			}
			f.s.store.UpdateAccount("fixture.chatgpt.session", map[string]any{"codex_credential": credential})
			code, _ := f.call(t, "POST", "/api/accounts/codex/sync", map[string]any{"account_ref": "cfm-one", "cpa_pool_id": "fixture-pool"}, "fixture-admin")
			account, _ := f.s.accountByRef("cfm-one")
			saved := mapValue(account["codex_credential"])
			if len(f.downloads) != tc.wantDownloads {
				t.Fatalf("unexpected checks: %v", f.downloads)
			}
			if tc.want == "" {
				if code != 502 || len(f.uploaded) != 0 || !reflect.DeepEqual(saved, credential) || f.files[newName]["refresh_token"] != "remote-refreshed-token" {
					t.Fatal("name collision must preserve remote and local credentials")
				}
				return
			}
			if code != 200 || len(f.uploaded) != 1 || f.uploaded[0] != tc.want || saved["cpa_file_name"] != tc.want {
				t.Fatalf("unexpected migration: HTTP %d, files %v, saved name %v", code, f.uploaded, saved["cpa_file_name"])
			}
			if tc.want == newName {
				code, _ = f.call(t, "POST", "/api/accounts/codex/sync", map[string]any{"account_ref": "cfm-one", "cpa_pool_id": "fixture-pool"}, "fixture-admin")
				if code != 200 || len(f.downloads) != 2 || len(f.uploaded) != 2 || f.uploaded[1] != newName {
					t.Fatal("repeat sync must reuse migrated name without another migration check")
				}
			}
		})
	}
}

func TestCodexCPALegacySyncCheckFailureDoesNotUploadOrChangeRecord(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500, 302, 204} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := newCPABindingFixture(t)
			requests := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodGet || r.URL.Path != "/v0/management/auth-files/download" {
					t.Error("check failure must not upload")
				}
				w.Header().Set("Location", "/must-not-follow")
				w.WriteHeader(status)
			}))
			defer upstream.Close()
			f.s.external.saveCPAList([]externalCPAPool{{ID: "fixture-pool", BaseURL: upstream.URL, SecretKey: "fixture-cpa-admin"}})
			credential := map[string]any{"access_token": "fixture-new-access", "refresh_token": "fixture-new-refresh", "email": "person@example.test", "account_id": "workspace-one", "cpa_pool_id": "fixture-pool", "cpa_file_name": "codex-person-example.test.json"}
			f.s.store.UpdateAccount("fixture.chatgpt.session", map[string]any{"codex_credential": credential})
			code, _ := f.call(t, "POST", "/api/accounts/codex/sync", map[string]any{"account_ref": "cfm-one", "cpa_pool_id": "fixture-pool"}, "fixture-admin")
			account, _ := f.s.accountByRef("cfm-one")
			if code != 502 || requests != 1 || !reflect.DeepEqual(mapValue(account["codex_credential"]), credential) {
				t.Fatal("failed check changed credentials or followed redirect")
			}
		})
	}
}

func TestCodexCPAFileCheckNetworkFailureAndCancellation(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	upstream.Close()
	pool := externalCPAPool{BaseURL: upstream.URL}
	if exists, err := codexCPAFileExists(context.Background(), pool, "old.json"); exists || err == nil {
		t.Fatal("network failure must not be treated as missing file")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if exists, err := codexCPAFileExists(ctx, pool, "old.json"); exists || err == nil {
		t.Fatal("cancelled check must fail")
	}
}

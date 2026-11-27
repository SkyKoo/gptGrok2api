package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mailboxstore "github.com/auucoder/gptgrok2api-go/internal/mailbox"
	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

func TestValidateHMESourceConfig(t *testing.T) {
	if msg := validateHMESourceConfig("http://hme:8081", "acc_1", "secret", true); msg != "" {
		t.Fatalf("valid config rejected: %s", msg)
	}
	if msg := validateHMESourceConfig("ftp://hme:8081", "acc_1", "secret", true); msg == "" {
		t.Fatal("invalid scheme accepted")
	}
	if msg := validateHMESourceConfig("http://hme:8081", "", "secret", true); msg == "" {
		t.Fatal("missing account_id accepted")
	}
	if msg := validateHMESourceConfig("http://hme:8081", "acc_1", "", true); msg == "" {
		t.Fatal("missing password accepted")
	}
	if msg := validateHMESourceConfig("", "", "", false); msg != "" {
		t.Fatalf("empty disabled config rejected: %s", msg)
	}
}

func TestResolveAccountMailboxSourceIDMatchesLegacyAccountAcrossMultipleSources(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	s.mailboxManager = mailboxstore.NewResourceManager(nil, newMailboxProvider)
	s.mailboxManager.SetSourceLoader(func(context.Context) ([]mailboxstore.Source, error) {
		return []mailboxstore.Source{
			{ID: "hme-first", Type: "icloud_hme", Enabled: true, Config: map[string]any{"api_base": "http://hme:8081", "account_id": "acc_first", "admin_password": "secret"}},
			{ID: "hme-second", Type: "icloud_hme", Enabled: true, Config: map[string]any{"api_base": "http://hme:8081", "account_id": "acc_second", "admin_password": "secret"}},
		}, nil
	})
	legacy := registerruntime.Mailbox{Email: "old@icloud.com", AccountID: "acc_first", SourceType: "hme"}
	if got := s.resolveAccountMailboxSourceID(context.Background(), legacy, "hme-second"); got != "hme-first" {
		t.Fatalf("got=%q want=hme-first", got)
	}
	explicit := registerruntime.Mailbox{Email: "new@icloud.com", AccountID: "acc_second", SourceID: "hme-second", SourceType: "hme"}
	if got := s.resolveAccountMailboxSourceID(context.Background(), explicit, "hme-first"); got != "hme-second" {
		t.Fatalf("got=%q want=hme-second", got)
	}
}

func TestICloudMailboxSourcesCreateMultiAccountAndPinChannels(t *testing.T) {
	s, _ := mailboxHTTPTestServer(t)
	ctx := context.Background()

	// Seed migrated non-HME source plus one existing icloud_hme source.
	if _, err := s.mailboxStore.UpsertSource(ctx, mailboxstore.Source{
		ID: "cloudmail_gen-1", Type: "cloudmail_gen", Label: "邮箱来源 1", Enabled: false,
		Config: map[string]any{"id": "cloudmail_gen-1", "type": "cloudmail_gen", "label": "邮箱来源 1", "enable": false},
	}); err != nil {
		t.Fatal(err)
	}
	firstConfig := map[string]any{
		"id": "icloud_hme-2", "type": "icloud_hme", "label": "邮箱来源 2", "enable": true,
		"api_base": "http://hme:8081", "account_id": "acc_primary", "admin_password": "PRIVATE-HME-PASS",
	}
	if _, err := s.mailboxStore.UpsertSource(ctx, mailboxSourceFromProvider(firstConfig, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.registerStore.Update(map[string]any{
		"mail": map[string]any{"providers": []any{firstConfig}},
		"target_settings": map[string]any{
			"openai": map[string]any{"mail_source_id": ""},
			"grok":   map[string]any{"mail_source_id": ""},
			"adobe":  map[string]any{"mail_source_id": "icloud_hme-2"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Reject unsupported source type on create.
	badType := adminRequest(s.Handler(), http.MethodPost, "/api/icloud/sources", strings.NewReader(`{"source":{"type":"cloudmail_gen","label":"Unsupported"}}`))
	if badType.Code != http.StatusBadRequest {
		t.Fatalf("unsupported create status=%d body=%s", badType.Code, badType.Body.String())
	}

	// Reject duplicate (api_base, account_id) regardless of trailing slash / host case.
	dup := adminRequest(s.Handler(), http.MethodPost, "/api/icloud/sources", strings.NewReader(`{"source":{"type":"icloud_hme","label":"重复来源","enable":true,"config":{"api_base":"HTTP://HME:8081/","account_id":"acc_primary","admin_password":"PRIVATE-HME-PASS"}}}`))
	if dup.Code != http.StatusConflict {
		t.Fatalf("duplicate status=%d body=%s", dup.Code, dup.Body.String())
	}

	// Create second icloud_hme source on the same HME instance with a different account_id.
	createRes := adminRequest(s.Handler(), http.MethodPost, "/api/icloud/sources", strings.NewReader(`{"source":{"type":"icloud_hme","label":"iCloud 副号","enable":true,"config":{"api_base":" http://hme:8081/ ","account_id":" acc_secondary ","admin_password":"PRIVATE-HME-PASS","delete_hme_mailbox_on_account_delete":true}}}`))
	if createRes.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", createRes.Code, createRes.Body.String())
	}
	if strings.Contains(createRes.Body.String(), "PRIVATE-HME-PASS") {
		t.Fatal("create response leaked HME password")
	}
	var created struct {
		Source map[string]any `json:"source"`
	}
	if err := json.Unmarshal(createRes.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	secondID := stringValue(created.Source["id"])
	if !strings.HasPrefix(secondID, "icloud_hme-") {
		t.Fatalf("unexpected generated id=%q", secondID)
	}
	if created.Source["label"] != "iCloud 副号" || created.Source["status"] != "available" || created.Source["enable"] != true {
		t.Fatalf("unexpected created source: %+v", created.Source)
	}
	cfgMap := mapValue(created.Source["config"])
	if cfgMap["api_base"] != "http://hme:8081" || cfgMap["account_id"] != "acc_secondary" || cfgMap["admin_password_set"] != true {
		t.Fatalf("unexpected created config: %+v", cfgMap)
	}

	// Stored config must not persist admin_password_set.
	storedSecond, err := s.mailboxStore.GetSource(ctx, secondID)
	if err != nil {
		t.Fatal(err)
	}
	if _, leaked := storedSecond.Config["admin_password_set"]; leaked {
		t.Fatalf("stored config kept admin_password_set: %+v", storedSecond.Config)
	}

	// Empty channels must have been pinned to the original single source so existing parsing keeps working.
	for _, target := range []string{"openai", "grok", "adobe"} {
		targetCfg := s.registerStore.ConfigForTarget(target)
		if got := stringValue(mapValue(targetCfg["mail"])["source_id"]); got != "icloud_hme-2" {
			t.Fatalf("target %s source_id=%q want=icloud_hme-2", target, got)
		}
	}
	if freeCfg, err := registerruntime.ParseFreeConfig(s.registerStore.ConfigForTarget("openai")); err != nil || freeCfg.HME.AccountID != "acc_primary" {
		t.Fatalf("ParseFreeConfig after 2nd source failed: cfg=%+v err=%v", freeCfg, err)
	}

	// Switching Grok to the second source selects acc_secondary while OpenAI remains on acc_primary.
	switchRes := adminRequest(s.Handler(), http.MethodPost, "/api/register?target=grok", strings.NewReader(`{"target":"grok","mail":{"source_id":"`+secondID+`"}}`))
	if switchRes.Code != http.StatusOK {
		t.Fatalf("switch grok source status=%d body=%s", switchRes.Code, switchRes.Body.String())
	}
	if grokCfg, err := registerruntime.ParseGrokConfig(s.registerStore.ConfigForTarget("grok")); err != nil || grokCfg.HME.AccountID != "acc_secondary" || grokCfg.MailboxSourceID != secondID {
		t.Fatalf("ParseGrokConfig did not select second source: cfg=%+v err=%v", grokCfg, err)
	}
	if freeCfg, err := registerruntime.ParseFreeConfig(s.registerStore.ConfigForTarget("openai")); err != nil || freeCfg.HME.AccountID != "acc_primary" {
		t.Fatalf("OpenAI source changed unexpectedly: cfg=%+v err=%v", freeCfg, err)
	}

	// Updating same api_base with blank password retains password; changing api_base with blank password fails.
	editSameBase := adminRequest(s.Handler(), http.MethodPost, "/api/icloud/sources", strings.NewReader(`{"source":{"id":"`+secondID+`","type":"icloud_hme","label":"iCloud 副号-更新","enable":true,"config":{"api_base":"http://hme:8081","account_id":"acc_secondary","admin_password":"","admin_password_set":true}}}`))
	if editSameBase.Code != http.StatusOK {
		t.Fatalf("edit same api_base failed: %d %s", editSameBase.Code, editSameBase.Body.String())
	}
	editChangedBase := adminRequest(s.Handler(), http.MethodPost, "/api/icloud/sources", strings.NewReader(`{"source":{"id":"`+secondID+`","type":"icloud_hme","label":"iCloud 副号-换地址","enable":true,"config":{"api_base":"http://hme-other:8081","account_id":"acc_secondary","admin_password":""}}}`))
	if editChangedBase.Code != http.StatusBadRequest {
		t.Fatalf("changed api_base without password should fail, got %d %s", editChangedBase.Code, editChangedBase.Body.String())
	}

	// Legacy non-HME migrated source can still be saved.
	saveLegacy := adminRequest(s.Handler(), http.MethodPost, "/api/icloud/sources", strings.NewReader(`{"source":{"id":"cloudmail_gen-1","type":"cloudmail_gen","label":"邮箱来源 1","enable":false,"config":{}}}`))
	if saveLegacy.Code != http.StatusOK {
		t.Fatalf("save legacy source failed: %d %s", saveLegacy.Code, saveLegacy.Body.String())
	}
}

func TestICloudMailboxSourceCreateRetriesAfterConfigWriteFailure(t *testing.T) {
	s, _ := mailboxHTTPTestServer(t)
	ctx := context.Background()
	handler := s.Handler()
	post := func(body string) *httptest.ResponseRecorder {
		return adminRequest(handler, http.MethodPost, "/api/icloud/sources", strings.NewReader(body))
	}
	primary := post(`{"source":{"id":"primary","config":{"api_base":"http://hme:8081","account_id":"acc_primary","admin_password":"fixture"}}}`)
	if primary.Code != http.StatusOK {
		t.Fatalf("seed primary: %d %s", primary.Code, primary.Body.String())
	}
	legacy, err := s.mailboxStore.Upsert(ctx, mailboxstore.Mailbox{
		Email: "secondary@example.test", SourceType: "hme", SourceAccountID: "acc_secondary",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	configStore := s.registerStore
	invalidParent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(invalidParent, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	s.registerStore = registerruntime.New(filepath.Join(invalidParent, "register.json"), filepath.Join(t.TempDir(), "accounts.json"))
	const request = `{"source":{"config":{"api_base":"http://hme:8081","account_id":"acc_secondary","admin_password":"fixture"}}}`
	failed := post(request)
	s.registerStore = configStore
	if failed.Code != http.StatusInternalServerError {
		t.Fatalf("expected configuration write failure: %d %s", failed.Code, failed.Body.String())
	}
	sources, err := s.mailboxStore.ListSources(ctx)
	if err != nil || len(sources) != 2 {
		t.Fatalf("expected primary and a persisted secondary: count=%d err=%v", len(sources), err)
	}
	var secondID string
	for _, source := range sources {
		if source.Config["account_id"] == "acc_secondary" {
			secondID = source.ID
		}
	}
	if secondID == "" || mailboxView(t, s, legacy.ID).SourceID != "" {
		t.Fatal("source must persist for retry without binding the legacy mailbox yet")
	}
	for range 2 {
		res := post(request)
		if res.Code != http.StatusOK {
			t.Fatalf("retry: %d %s", res.Code, res.Body.String())
		}
		var result struct {
			Source struct{ ID string } `json:"source"`
		}
		if err := json.Unmarshal(res.Body.Bytes(), &result); err != nil || result.Source.ID != secondID {
			t.Fatalf("retry did not reuse the saved source: id=%q err=%v", result.Source.ID, err)
		}
	}
	conflict := post(strings.Replace(request, "\"fixture\"", "\"different-password\"", 1))
	if conflict.Code != http.StatusConflict {
		t.Fatalf("different credentials must not overwrite an existing source: %d", conflict.Code)
	}
	sources, err = s.mailboxStore.ListSources(ctx)
	providers, _ := mapValue(s.registerStore.Get()["mail"])["providers"].([]any)
	if err != nil || len(sources) != 2 || len(providers) != 2 {
		t.Fatalf("retry duplicated a source: database=%d config=%d err=%v", len(sources), len(providers), err)
	}
	for _, target := range []string{"openai", "grok", "adobe"} {
		if got := mapValue(s.registerStore.ConfigForTarget(target)["mail"])["source_id"]; got != "primary" {
			t.Fatalf("%s lost its original source: %v", target, got)
		}
	}
	if got := mailboxView(t, s, legacy.ID).SourceID; got != secondID {
		t.Fatalf("retry did not bind a unique legacy mailbox: %q", got)
	}
}

func TestICloudMailboxSourceBackfillRequiresUniqueAccountOnSaveAndMigration(t *testing.T) {
	s, _ := mailboxHTTPTestServer(t)
	ctx := context.Background()
	_, err := s.mailboxStore.UpsertSource(ctx, mailboxstore.Source{ID: "disabled", Type: "icloud_hme", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	for _, tc := range []struct {
		name, kind, account, existing, want string
	}{
		{name: "unique", kind: "hme", account: "acc_unique", want: "unique"},
		{name: "ambiguous", kind: "hme", account: "acc_shared"},
		{name: "missing", kind: "hme"},
		{name: "unknown", kind: "hme", account: "acc_unknown"},
		{name: "wrong-provider", kind: "other", account: "acc_unique"},
		{name: "already-bound", kind: "hme", account: "acc_unique", existing: "previous", want: "previous"},
	} {
		email := tc.name + "@example.test"
		want[email] = tc.want
		if _, err := s.mailboxStore.Upsert(ctx, mailboxstore.Mailbox{
			Email: email, SourceType: tc.kind, SourceAccountID: tc.account, SourceID: tc.existing,
		}, "test"); err != nil {
			t.Fatal(err)
		}
	}
	saveDisabled := adminRequest(s.Handler(), http.MethodPost, "/api/icloud/sources", strings.NewReader(`{"source":{"id":"disabled","label":"rename only","enable":false}}`))
	if saveDisabled.Code != http.StatusOK {
		t.Fatalf("save disabled source: %d %s", saveDisabled.Code, saveDisabled.Body.String())
	}
	providers := []any{map[string]any{"id": "disabled", "type": "icloud_hme", "enable": false}}
	for _, item := range []struct{ id, host, account string }{
		{id: "shared-a", host: "http://hme-a:8081", account: "acc_shared"},
		{id: "shared-b", host: "http://hme-b:8081", account: "acc_shared"},
		{id: "unique", host: "http://hme-a:8081", account: "acc_unique"},
	} {
		providers = append(providers, map[string]any{"id": item.id, "type": "icloud_hme", "api_base": item.host, "account_id": item.account})
	}
	if _, err := s.registerStore.Update(map[string]any{"mail": map[string]any{"providers": providers}}); err != nil {
		t.Fatal(err)
	}
	if err := s.syncMailboxSources(ctx); err != nil {
		t.Fatal(err)
	}
	views, total, err := s.mailboxStore.List(ctx, 1, 20, mailboxstore.MailboxListFilter{})
	if err != nil || total != 6 {
		t.Fatalf("list mailbox assets: total=%d err=%v", total, err)
	}
	for _, view := range views {
		if view.SourceID != want[view.Email] {
			t.Errorf("%s source_id=%q, want %q", view.Email, view.SourceID, want[view.Email])
		}
	}
}

func TestICloudMailboxSourceRoutesSecondAccountCreateReadAndDelete(t *testing.T) {
	s, _ := mailboxHTTPTestServer(t)
	const address = "secondary@example.test"
	const upstreamAliasID = "secondary-anonymous-id"
	const fixtureCode = "123456"
	received := make(chan string, 5)
	hme := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		message := map[string]any{"id": "message-1", "folder": "inbox", "to": address,
			"from": "fixture@example.test", "date": time.Now().UTC().Format(time.RFC3339)}
		var data any
		switch r.URL.Path {
		case "/api/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "hme_session", Value: "fixture", Path: "/", HttpOnly: true})
			data = map[string]string{"csrf_token": "fixture-csrf"}
		case "/api/create":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				http.Error(w, "invalid fixture request", http.StatusBadRequest)
				return
			}
			received <- "create:" + body["account_id"]
			data = map[string]string{"email": address, "account_id": body["account_id"]}
		case "/api/inbox":
			received <- "inbox:" + r.URL.Query().Get("account_id")
			if r.URL.Query().Get("alias") != address {
				t.Error("inbox request did not use the created alias")
			}
			data = map[string]any{"method": "imap", "messages": []any{message}}
		case "/api/inbox/message-1":
			received <- "detail:" + r.URL.Query().Get("account_id")
			message["body"] = "Verification code " + fixtureCode
			data = message
		case "/api/aliases":
			accountID := r.URL.Query().Get("account_id")
			received <- "aliases:" + accountID
			if accountID != "acc_secondary" {
				http.Error(w, "wrong fixture account", http.StatusBadRequest)
				return
			}
			data = map[string]any{"aliases": []map[string]string{{"anonymousId": upstreamAliasID, "email": address}}}
		case "/api/aliases/" + upstreamAliasID:
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				http.Error(w, "invalid fixture request", http.StatusBadRequest)
				return
			}
			received <- "delete:" + body["account_id"]
			if r.Method != http.MethodDelete || body["account_id"] != "acc_secondary" {
				http.Error(w, "wrong fixture delete", http.StatusBadRequest)
				return
			}
			data = map[string]bool{"deleted": true}
		default:
			t.Errorf("unexpected HME fixture route: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
	}))
	defer hme.Close()

	handler := s.Handler()
	for _, id := range []string{"primary", "secondary"} {
		body, err := json.Marshal(map[string]any{"source": map[string]any{"id": id,
			"config": map[string]any{"api_base": hme.URL, "account_id": "acc_" + id, "admin_password": "fixture"}}})
		if err != nil {
			t.Fatal(err)
		}
		res := adminRequest(handler, http.MethodPost, "/api/icloud/sources", strings.NewReader(string(body)))
		if res.Code != http.StatusOK {
			t.Fatalf("create %s source: %d %s", id, res.Code, res.Body.String())
		}
	}
	selected := adminRequest(handler, http.MethodPost, "/api/register?target=grok", strings.NewReader(`{"target":"grok","mail":{"source_id":"secondary"}}`))
	if selected.Code != http.StatusOK {
		t.Fatalf("select second source: %d %s", selected.Code, selected.Body.String())
	}
	cfg, err := registerruntime.ParseGrokConfig(s.registerStore.ConfigForTarget("grok"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	source, err := newMailboxResourceSource(ctx, s.mailboxManager, "grok", cfg.MailboxSourceID, cfg.PollInterval)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	box, err := source.Acquire(ctx, "second-account-test")
	if err != nil || box.SourceID != "secondary" || box.AccountID != "acc_secondary" {
		t.Fatalf("unexpected mailbox identity: source=%q account=%q err=%v", box.SourceID, box.AccountID, err)
	}
	saved, err := s.mailboxStore.Get(ctx, box.ID)
	if err != nil || saved.SourceID != "secondary" || saved.SourceAccountID != "acc_secondary" {
		t.Fatalf("unexpected stored mailbox identity: source=%q account=%q err=%v", saved.SourceID, saved.SourceAccountID, err)
	}
	code, err := source.WaitCodeFor(ctx, box, time.Now().Add(-time.Minute), func(sender string) bool {
		return sender == "fixture@example.test"
	})
	if err != nil || code != fixtureCode {
		t.Fatalf("did not receive the fixture email code: %v", err)
	}
	deletePath := "/api/icloud/mailboxes/" + box.ID
	occupied := adminRequest(handler, http.MethodDelete, deletePath, nil)
	if occupied.Code != http.StatusConflict || len(received) != 3 {
		t.Fatalf("an in-use mailbox must be protected before calling HME: %d %s", occupied.Code, occupied.Body.String())
	}
	if err := source.Release(ctx, box, "test"); err != nil {
		t.Fatal(err)
	}
	// 邮箱删除必须读资产归属，即使当前渠道的注册来源已改回主号。
	primarySelected := adminRequest(handler, http.MethodPost, "/api/register?target=grok", strings.NewReader(`{"target":"grok","mail":{"source_id":"primary"}}`))
	if primarySelected.Code != http.StatusOK {
		t.Fatalf("switch back to the first source: %d %s", primarySelected.Code, primarySelected.Body.String())
	}
	deleted := adminRequest(handler, http.MethodDelete, deletePath, nil)
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete second-account mailbox: %d %s", deleted.Code, deleted.Body.String())
	}
	saved, err = s.mailboxStore.Get(ctx, box.ID)
	if err != nil || saved.Status != "deleted" || saved.SourceAccountID != "acc_secondary" {
		t.Fatalf("unexpected mailbox after deletion: status=%q account=%q err=%v", saved.Status, saved.SourceAccountID, err)
	}
	for _, want := range []string{"create:acc_secondary", "inbox:acc_secondary", "detail:acc_secondary", "aliases:acc_secondary", "delete:acc_secondary"} {
		select {
		case got := <-received:
			if got != want {
				t.Errorf("HME request=%q, want %q", got, want)
			}
		default:
			t.Fatalf("missing HME request %q", want)
		}
	}
}

func TestICloudMailboxReconcileSyncsAllEnabledHMESources(t *testing.T) {
	s, _ := mailboxHTTPTestServer(t)
	ctx := context.Background()

	var failSecond bool
	hme := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respond := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": v}) }
		switch r.URL.Path {
		case "/api/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "hme_session", Value: "fixture", Path: "/", HttpOnly: true})
			respond(map[string]string{"csrf_token": "fixture-csrf"})
		case "/api/aliases":
			accountID := r.URL.Query().Get("account_id")
			switch accountID {
			case "acc_one":
				respond(map[string]any{"aliases": []map[string]string{
					{"anonymousId": "anon-1", "email": "one@icloud.com"},
				}})
			case "acc_two":
				if failSecond {
					w.WriteHeader(http.StatusBadGateway)
					_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "code": "UPSTREAM_UNAVAILABLE"})
					return
				}
				respond(map[string]any{"aliases": []map[string]string{
					{"anonymousId": "anon-2", "email": "two@icloud.com"},
				}})
			default:
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "code": "ACCOUNT_NOT_FOUND"})
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer hme.Close()

	// Insert an enabled non-HME source first so naive Open("") would fail on it.
	if _, err := s.mailboxStore.UpsertSource(ctx, mailboxstore.Source{
		ID: "aaa-legacy", Type: "cloudmail_gen", Label: "旧来源", Enabled: true,
		Config: map[string]any{"id": "aaa-legacy", "type": "cloudmail_gen", "enable": true},
	}); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		id, label, account string
		enabled            bool
	}{
		{"hme-1", "主号来源", "acc_one", true},
		{"hme-2", "副号来源", "acc_two", true},
		{"hme-disabled", "停用来源", "acc_off", false},
	} {
		cfg := map[string]any{"id": item.id, "type": "icloud_hme", "label": item.label, "enable": item.enabled, "api_base": hme.URL, "account_id": item.account, "admin_password": "secret"}
		if _, err := s.mailboxStore.UpsertSource(ctx, mailboxSourceFromProvider(cfg, 0)); err != nil {
			t.Fatal(err)
		}
	}

	// Full multi-source reconcile syncs aliases from both enabled HME accounts.
	res := adminRequest(s.Handler(), http.MethodPost, "/api/icloud/reconcile", strings.NewReader(`{}`))
	if res.Code != http.StatusOK {
		t.Fatalf("reconcile status=%d body=%s", res.Code, res.Body.String())
	}
	var out struct {
		Success bool     `json:"success"`
		Outcome string   `json:"outcome"`
		Aliases int      `json:"aliases"`
		Errors  []string `json:"errors"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Success || out.Outcome != "success" || out.Aliases != 2 || len(out.Errors) != 0 {
		t.Fatalf("unexpected multi-source reconcile result: %+v", out)
	}

	views, total, err := s.mailboxStore.List(ctx, 1, 20, mailboxstore.MailboxListFilter{})
	if err != nil || total != 2 {
		t.Fatalf("total=%d err=%v", total, err)
	}
	byEmail := map[string]mailboxstore.MailboxView{}
	for _, view := range views {
		byEmail[view.Email] = view
	}
	if byEmail["one@icloud.com"].SourceID != "hme-1" || byEmail["one@icloud.com"].SourceAccountID != "acc_one" {
		t.Fatalf("first mailbox source mismatch: %+v", byEmail["one@icloud.com"])
	}
	if byEmail["two@icloud.com"].SourceID != "hme-2" || byEmail["two@icloud.com"].SourceAccountID != "acc_two" {
		t.Fatalf("second mailbox source mismatch: %+v", byEmail["two@icloud.com"])
	}

	// Partial failure when one HME account fails still syncs the healthy source and tags the failing source label.
	failSecond = true
	partialRes := adminRequest(s.Handler(), http.MethodPost, "/api/icloud/reconcile", strings.NewReader(`{}`))
	if partialRes.Code != http.StatusOK {
		t.Fatalf("partial reconcile status=%d body=%s", partialRes.Code, partialRes.Body.String())
	}
	out = struct {
		Success bool     `json:"success"`
		Outcome string   `json:"outcome"`
		Aliases int      `json:"aliases"`
		Errors  []string `json:"errors"`
	}{}
	if err := json.Unmarshal(partialRes.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Success || out.Outcome != "partial" || out.Aliases != 1 || len(out.Errors) != 1 || !strings.Contains(out.Errors[0], "副号来源") {
		t.Fatalf("unexpected partial reconcile result: %+v", out)
	}
	_ = time.Second
}

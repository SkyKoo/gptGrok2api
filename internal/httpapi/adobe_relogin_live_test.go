package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
	mailboxstore "github.com/auucoder/gptgrok2api-go/internal/mailbox"
)

// TestAdobeReloginLivePrivate is opt-in and must only target an isolated local
// Worker containing a COPY of the original profile. It executes one real login;
// the production Store and browser callback are unchanged. Only email delivery
// is replaced by a local HME endpoint fed with a user-provided OTP file.
//
// Required: CFM_ADOBE_RELOGIN_LIVE_PRIVATE_DIR points to a 0700 directory with
// 0600 account.private.json (adobe.Account or an exact-one accounts envelope),
// worker.env, proxy.txt, profile-id.txt.
// Write a fresh 0600 otp.txt containing six digits only after result.json reports
// awaiting_otp. start.marker is deliberately never removed: an interrupted or
// failed run must not silently repeat login. Results and updated credentials use
// separate files; the source account file is never overwritten.
func TestAdobeReloginLivePrivate(t *testing.T) {
	privateDir := os.Getenv("CFM_ADOBE_RELOGIN_LIVE_PRIVATE_DIR")
	if privateDir == "" {
		t.Skip("live Adobe relogin is opt-in")
	}
	privateDir = filepath.Clean(privateDir)
	info, err := os.Lstat(privateDir)
	if err != nil || !filepath.IsAbs(privateDir) || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatal("live_private_directory_invalid")
	}
	read := func(name string) []byte {
		data, _, err := adobeLivePrivateRead(filepath.Join(privateDir, name), 2<<20)
		if err != nil {
			t.Fatalf("live_private_input_invalid: %s", name)
		}
		return data
	}
	sourceRaw := read("account.private.json")
	originalDigest := sha256.Sum256(sourceRaw)
	original, err := adobeLiveAccountInput(sourceRaw)
	if err != nil || original.Email == "" || original.AccountID == "" || original.Password == "" || original.ProfileID == "" {
		t.Fatal("live_account_input_invalid")
	}
	profileID := strings.TrimSpace(string(read("profile-id.txt")))
	if !regexp.MustCompile(`^relogin-[A-Za-z0-9_-]{8,100}$`).MatchString(profileID) || profileID == original.ProfileID {
		t.Fatal("live_profile_copy_required")
	}
	workerURL, workerKey, err := adobeLiveWorkerConfig(read("worker.env"))
	if err != nil {
		t.Fatal("live_worker_configuration_invalid")
	}
	proxy := strings.TrimSpace(string(read("proxy.txt")))
	if proxy == "" || strings.ContainsAny(proxy, "\r\n\x00") {
		t.Fatal("live_proxy_configuration_invalid")
	}
	if proxy != "direct" {
		p, parseErr := url.Parse(proxy)
		if parseErr != nil || p.Host == "" || (p.Scheme != "http" && p.Scheme != "https" && p.Scheme != "socks5" && p.Scheme != "socks5h") {
			t.Fatal("live_proxy_configuration_invalid")
		}
	}
	for _, name := range []string{"start.marker", "account.updated.private.json", "otp.txt"} {
		if _, err := os.Lstat(filepath.Join(privateDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("live_existing_input_prevents_replay: %s", name)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	started := time.Now().UTC()
	var logins, inboxes, details, aliases, rejected atomic.Int32
	var refreshCalls, otpContinuations, workerReservations, workerReleases atomic.Int32
	var emailStarted, emailSubmitted, passwordStarted, passwordSubmitted, otpStarted, otpSubmitted atomic.Bool
	var workerStage, workerLease atomic.Value
	workerStage.Store("")
	workerLease.Store("")
	var outputMu sync.Mutex
	writeStatus := func(phase string, done, ok, awaiting, supplied, unchanged bool, failure string) {
		outputMu.Lock()
		defer outputMu.Unlock()
		result := map[string]any{"phase": phase, "done": done, "ok": ok, "awaiting_otp": awaiting,
			"otp_supplied": supplied, "source_unchanged": unchanged,
			"hme_logins": logins.Load(), "hme_inbox_polls": inboxes.Load(), "hme_details": details.Load(),
			"hme_alias_queries": aliases.Load(), "hme_rejected_requests": rejected.Load(),
			"elapsed_seconds": int(time.Since(started).Seconds()), "error_code": failure,
			"worker_stage": workerStage.Load(), "refresh_calls": refreshCalls.Load(), "otp_continuations": otpContinuations.Load(),
			"worker_reservations": workerReservations.Load(), "worker_releases": workerReleases.Load(),
			"actual_email_submission_started": emailStarted.Load(), "actual_email_form_submitted": emailSubmitted.Load(),
			"actual_password_submission_started": passwordStarted.Load(), "actual_password_form_submitted": passwordSubmitted.Load(),
			"actual_otp_submission_started": otpStarted.Load(), "actual_otp_form_submitted": otpSubmitted.Load()}
		data, marshalErr := json.MarshalIndent(result, "", "  ")
		if marshalErr != nil || adobeLivePrivateWrite(filepath.Join(privateDir, "result.json"), data, false) != nil {
			t.Error("live_status_write_failed")
			cancel()
		}
	}
	const sourceID, hmeAccount, aliasID = "fixture-hme", "fixture-hme-account", "fixture-alias"
	const hmePassword, hmeCookie = "local-fixture-only", "local-fixture-session"
	var supplied atomic.Bool
	var otpMu sync.Mutex
	var otpCode, otpTime string
	loadOTP := func() bool {
		otpMu.Lock()
		defer otpMu.Unlock()
		if otpCode != "" {
			return true
		}
		data, stat, readErr := adobeLivePrivateRead(filepath.Join(privateDir, "otp.txt"), 32)
		if errors.Is(readErr, os.ErrNotExist) {
			return false
		}
		value := strings.TrimSpace(string(data))
		if readErr != nil || stat == nil || stat.ModTime().Before(started) || stat.ModTime().After(time.Now().Add(time.Minute)) || !regexp.MustCompile(`^[0-9]{6}$`).MatchString(value) {
			rejected.Add(1)
			cancel()
			return false
		}
		otpCode, otpTime = value, stat.ModTime().UTC().Format(time.RFC3339Nano)
		return true
	}
	hme := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		respond := func(data any) { _ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data}) }
		deny := func() {
			rejected.Add(1)
			cancel()
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"success":false,"error":"fixture_scope_rejected"}`)
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/auth/login" {
			var body map[string]string
			if json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&body) != nil || body["password"] != hmePassword || len(body) != 1 {
				deny()
				return
			}
			logins.Add(1)
			http.SetCookie(w, &http.Cookie{Name: "hme_session", Value: hmeCookie, Path: "/", HttpOnly: true})
			respond(map[string]string{"csrf_token": "fixture-csrf"})
			return
		}
		cookie, cookieErr := r.Cookie("hme_session")
		if cookieErr != nil || cookie.Value != hmeCookie || r.Method != http.MethodGet || r.URL.Query().Get("account_id") != hmeAccount {
			deny()
			return
		}
		switch r.URL.Path {
		case "/api/aliases":
			aliases.Add(1)
			respond(map[string]any{"aliases": []map[string]string{{"id": aliasID, "email": original.Email, "account_id": hmeAccount}}})
		case "/api/inbox":
			if r.URL.Query().Get("alias") != original.Email || r.URL.Query().Get("folder") != "all" {
				deny()
				return
			}
			inboxes.Add(1)
			available := loadOTP()
			writeStatus("awaiting_otp", false, false, !available, supplied.Load(), true, "")
			messages := []map[string]string{}
			if available {
				otpMu.Lock()
				messages = append(messages, map[string]string{"id": "live-adobe-otp", "folder": "inbox", "from": "Adobe <account-noreply@adobe.com>", "to": original.Email, "subject": "Adobe verification code", "date": otpTime})
				otpMu.Unlock()
			}
			respond(map[string]any{"method": "web_api", "messages": messages})
		case "/api/inbox/live-adobe-otp":
			if r.URL.Query().Get("folder") != "inbox" || r.URL.Query().Get("method") != "web_api" || !loadOTP() {
				deny()
				return
			}
			if details.Add(1) != 1 {
				deny()
				return
			}
			supplied.Store(true)
			writeStatus("otp_supplied", false, false, false, true, true, "")
			otpMu.Lock()
			defer otpMu.Unlock()
			respond(map[string]string{"id": "live-adobe-otp", "folder": "inbox", "from": "Adobe <account-noreply@adobe.com>", "to": original.Email, "date": otpTime, "body": "Your Adobe verification code is " + otpCode + "."})
		default:
			// In particular, never create, delete, or rotate a mailbox.
			deny()
		}
	}))
	defer hme.Close()
	// Transparent observer: forward every allowed request to the real local
	// Worker once. Inspect only fixed evidence in memory; never log or persist
	// request/response bodies, browser credentials, account IDs or lease IDs.
	workerClient := &http.Client{Timeout: 180 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer workerClient.CloseIdleConnections()
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deny := func() {
			rejected.Add(1)
			cancel()
			http.Error(w, "fixture_worker_scope_rejected", http.StatusForbidden)
		}
		if r.Header.Get("Authorization") != "Bearer "+workerKey || r.URL.RawQuery != "" {
			deny()
			return
		}
		body, readErr := io.ReadAll(io.LimitReader(r.Body, 2<<20+1))
		if readErr != nil || len(body) > 2<<20 {
			deny()
			return
		}
		isRun := r.URL.Path == "/v1/adobe/run" && r.Method == http.MethodPost
		switch {
		case r.URL.Path == "/v1/browser/reservations" && r.Method == http.MethodPost:
			var request map[string]any
			if json.Unmarshal(body, &request) != nil || request["provider"] != "adobe" || request["adobe_mode"] != "refresh" || request["profile_id"] != profileID || request["email"] != original.Email || request["account_id"] != original.AccountID || request["proxy"] != proxy {
				deny()
				return
			}
			id, _ := request["id"].(string)
			if !regexp.MustCompile(`^[a-f0-9]{48}$`).MatchString(id) || workerReservations.Add(1) != 1 {
				deny()
				return
			}
			workerLease.Store(id)
		case strings.HasPrefix(r.URL.Path, "/v1/browser/reservations/") && (r.Method == http.MethodGet || r.Method == http.MethodDelete):
			if workerLease.Load() == "" || r.URL.Path != "/v1/browser/reservations/"+workerLease.Load().(string) {
				deny()
				return
			}
			if r.Method == http.MethodDelete {
				workerReleases.Add(1)
			}
		case isRun:
			var request struct {
				Action string `json:"action"`
				Code   string `json:"code"`
				Lease  string `json:"browser_lease"`
			}
			if json.Unmarshal(body, &request) != nil || request.Action != "refresh" || request.Lease == "" || request.Lease != workerLease.Load() {
				deny()
				return
			}
			call := refreshCalls.Add(1)
			if (request.Code == "" && call != 1) || (request.Code != "" && (call != 2 || !regexp.MustCompile(`^[0-9]{6}$`).MatchString(request.Code))) {
				deny()
				return
			}
			if request.Code != "" && otpContinuations.Add(1) != 1 {
				deny()
				return
			}
		default:
			deny()
			return
		}
		upstream, requestErr := http.NewRequestWithContext(r.Context(), r.Method, workerURL+r.URL.Path, bytes.NewReader(body))
		if requestErr != nil {
			deny()
			return
		}
		upstream.Header.Set("Authorization", "Bearer "+workerKey)
		upstream.Header.Set("Content-Type", "application/json")
		response, requestErr := workerClient.Do(upstream)
		if requestErr != nil {
			http.Error(w, "fixture_worker_transport_failed", http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		data, readErr := io.ReadAll(io.LimitReader(response.Body, 2<<20+1))
		if readErr != nil || len(data) > 2<<20 {
			http.Error(w, "fixture_worker_response_invalid", http.StatusBadGateway)
			return
		}
		if isRun {
			var observed struct {
				Stage   string `json:"stage"`
				Account struct {
					SessionDiagnostics map[string]json.RawMessage `json:"session_diagnostics"`
				} `json:"account"`
			}
			if json.Unmarshal(data, &observed) == nil {
				switch observed.Stage {
				case "completed", "awaiting_verification", "needs_manual", "outcome_unknown":
					workerStage.Store(observed.Stage)
				}
				for key, target := range map[string]*atomic.Bool{
					"login_email_submission_started": &emailStarted, "login_email_submitted": &emailSubmitted,
					"login_password_submission_started": &passwordStarted, "login_password_submitted": &passwordSubmitted,
					"login_otp_submission_started": &otpStarted, "login_otp_submitted": &otpSubmitted,
				} {
					var value bool
					if json.Unmarshal(observed.Account.SessionDiagnostics[key], &value) == nil && value {
						target.Store(true)
					}
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(data)
	}))
	defer worker.Close()
	cfg := adminTestConfig(t.TempDir())
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal("live_store_secret_failed")
	}
	cfg.AdminKey, cfg.APIKey = hex.EncodeToString(secret), hex.EncodeToString(secret)
	s := NewWithWorkers(cfg, false)
	if _, err := s.registerStore.Update(map[string]any{"adobe": map[string]any{"api_base": worker.URL, "api_key": workerKey}}); err != nil {
		t.Fatal("live_worker_configuration_failed")
	}
	s.mailboxManager = mailboxstore.NewResourceManager(nil, newMailboxProvider)
	s.mailboxManager.SetSourceLoader(func(context.Context) ([]mailboxstore.Source, error) {
		return []mailboxstore.Source{{ID: sourceID, Type: "icloud_hme", Enabled: true, Config: map[string]any{"api_base": hme.URL, "admin_password": hmePassword, "account_id": hmeAccount}}}, nil
	})
	copyAccount := original
	copyAccount.ProfileID = profileID
	copyAccount.Proxy, copyAccount.ProxyPolicy, copyAccount.ProxyBinding = proxy, "explicit", nil
	copyAccount.RegistrationMailbox = map[string]any{"provider": "icloud_hme", "source_type": "icloud_hme", "platform": "adobe", "source_id": sourceID, "account_id": hmeAccount, "email": original.Email, "id": aliasID, "source_mailbox_id": aliasID}
	rows, err := s.adobeStore.Import([]adobe.Account{copyAccount})
	if err != nil || len(rows) != 1 {
		t.Fatal("live_temporary_account_import_failed")
	}
	s.adobeStore.SetBrowserRefresher(s.refreshAdobeBrowserAccount)
	marker := []byte(started.Format(time.RFC3339Nano) + "\n")
	if adobeLivePrivateWrite(filepath.Join(privateDir, "start.marker"), marker, true) != nil {
		t.Fatal("live_marker_prevents_replay")
	}
	writeStatus("browser_relogin_started", false, false, false, false, true, "")
	t.Log("phase=browser_relogin_started")
	updated, failure := s.adobeStore.ReloginRevision(ctx, rows[0].ID, rows[0].Revision)
	currentRaw, _, readErr := adobeLivePrivateRead(filepath.Join(privateDir, "account.private.json"), 2<<20)
	unchanged := readErr == nil && sha256.Sum256(currentRaw) == originalDigest
	if failure != nil || rejected.Load() != 0 || !unchanged {
		code := "live_relogin_failed"
		if failure != nil {
			safe := adobe.SafeError(failure)
			if regexp.MustCompile(`^adobe_[a-z0-9_]+( \(HTTP [0-9]{3}\))?( \[[a-z0-9_]+\])?$`).MatchString(safe) {
				code = safe
			}
		}
		writeStatus("failed", true, false, false, supplied.Load(), unchanged, code)
		t.Fatal(code)
	}
	if updated.AccountID != original.AccountID || !strings.EqualFold(updated.Email, original.Email) || updated.ProfileID != profileID || updated.MediaSessionStatus != "refreshed" || updated.Revision <= rows[0].Revision {
		writeStatus("failed", true, false, false, supplied.Load(), unchanged, "live_result_validation_failed")
		t.Fatal("live_result_validation_failed")
	}
	// A retained warm session is not evidence of password/OTP re-login.
	if !emailSubmitted.Load() || (!passwordSubmitted.Load() && !otpSubmitted.Load()) {
		writeStatus("failed", true, false, false, supplied.Load(), unchanged, "live_real_login_evidence_missing")
		t.Fatal("live_real_login_evidence_missing")
	}
	raw, err := adobeLiveAccountOutput(sourceRaw, updated)
	if err != nil || adobeLivePrivateWrite(filepath.Join(privateDir, "account.updated.private.json"), raw, true) != nil {
		writeStatus("failed", true, false, false, supplied.Load(), unchanged, "live_updated_account_write_failed")
		t.Fatal("live_updated_account_write_failed")
	}
	writeStatus("completed", true, true, false, supplied.Load(), unchanged, "")
	t.Log("phase=completed source_unchanged=true credentials_saved=true")
}

func adobeLiveAccountInput(data []byte) (adobe.Account, error) {
	var account adobe.Account
	var envelope map[string]json.RawMessage
	if json.Unmarshal(data, &envelope) != nil || envelope == nil {
		return account, errors.New("account_input_invalid")
	}
	if raw, present := envelope["accounts"]; present {
		var accounts []json.RawMessage
		if json.Unmarshal(raw, &accounts) != nil || len(accounts) != 1 {
			return account, errors.New("exactly_one_account_required")
		}
		data = accounts[0]
	}
	if json.Unmarshal(data, &account) != nil {
		return adobe.Account{}, errors.New("account_input_invalid")
	}
	return account, nil
}

func adobeLiveAccountOutput(source []byte, account adobe.Account) ([]byte, error) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(source, &envelope) != nil {
		return nil, errors.New("account_input_invalid")
	}
	if _, wrapped := envelope["accounts"]; wrapped {
		raw, err := json.Marshal([]adobe.Account{account})
		if err != nil {
			return nil, err
		}
		envelope["accounts"] = raw
		return json.MarshalIndent(envelope, "", "  ")
	}
	return json.MarshalIndent(account, "", "  ")
}

func adobeLivePrivateRead(name string, limit int64) ([]byte, os.FileInfo, error) {
	info, err := os.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > limit {
		return nil, nil, errors.New("private_input_invalid")
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, nil, errors.New("private_input_changed")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, nil, errors.New("private_input_unreadable")
	}
	return data, opened, nil
}

func adobeLivePrivateWrite(name string, data []byte, exclusive bool) error {
	if exclusive {
		file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, err = file.Write(data)
		if err == nil {
			err = file.Sync()
		}
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(name), ".relogin-status-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(file.Name(), name)
	}
	return err
}

func adobeLiveWorkerConfig(data []byte) (string, string, error) {
	values := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			return "", "", errors.New("worker_env_invalid")
		}
		if key == "CFM_BROWSER_WORKER_URL" || key == "CFM_BROWSER_WORKER_TOKEN" || key == "GROK_BROWSER_TOKEN" {
			if _, found := values[key]; found {
				return "", "", errors.New("worker_env_duplicate")
			}
			values[key] = strings.Trim(strings.TrimSpace(value), "\"'")
		}
	}
	base := values["CFM_BROWSER_WORKER_URL"]
	if base == "" {
		base = "http://cfm-adobe-relogin-worker:8899"
	}
	u, err := url.Parse(base)
	if scanner.Err() != nil || err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.Port() == "" ||
		(u.Hostname() != "cfm-adobe-relogin-worker" && u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1") {
		return "", "", errors.New("worker_endpoint_not_local")
	}
	key := values["CFM_BROWSER_WORKER_TOKEN"]
	if key == "" {
		key = values["GROK_BROWSER_TOKEN"]
	}
	if strings.TrimSpace(key) == "" || strings.ContainsAny(key, "\r\n\x00") {
		return "", "", errors.New("worker_key_invalid")
	}
	return strings.TrimRight(base, "/"), key, nil
}

// These guards are fully offline; no real account, OTP, or Worker is used.
func TestAdobeReloginLiveFixtureGuards(t *testing.T) {
	for _, input := range []string{`{"email":"fixture@example.test"}`, `{"accounts":[{"email":"fixture@example.test"}],"verified":false,"mailbox_source":"manual_fixture"}`} {
		if account, err := adobeLiveAccountInput([]byte(input)); err != nil || account.Email != "fixture@example.test" {
			t.Fatal("valid single account input rejected")
		}
	}
	for _, input := range []string{`{"accounts":[]}`, `{"accounts":null}`, `{"accounts":[{},{}]}`, `{"accounts":{}}`, `[]`, `null`} {
		if _, err := adobeLiveAccountInput([]byte(input)); err == nil {
			t.Fatal("ambiguous account input accepted")
		}
	}
	valid := []byte("CFM_BROWSER_WORKER_URL=http://cfm-adobe-relogin-worker:8899\nGROK_BROWSER_TOKEN=fixture-only\n")
	if _, _, err := adobeLiveWorkerConfig(valid); err != nil {
		t.Fatal("valid local worker rejected")
	}
	for _, input := range []string{
		"CFM_BROWSER_WORKER_URL=https://production.example.test\nGROK_BROWSER_TOKEN=fixture\n",
		"CFM_BROWSER_WORKER_URL=http://user:pass@localhost:8899\nGROK_BROWSER_TOKEN=fixture\n",
		"CFM_BROWSER_WORKER_URL=http://localhost:8899?token=fixture\nGROK_BROWSER_TOKEN=fixture\n",
		"CFM_BROWSER_WORKER_URL=http://localhost:8899\n",
		"GROK_BROWSER_TOKEN=one\nGROK_BROWSER_TOKEN=two\n",
	} {
		if _, _, err := adobeLiveWorkerConfig([]byte(input)); err == nil {
			t.Fatal("invalid worker configuration accepted")
		}
	}
	name := filepath.Join(t.TempDir(), "private.json")
	if err := adobeLivePrivateWrite(name, []byte("fixture"), true); err != nil {
		t.Fatal("private fixture creation failed")
	}
	if _, _, err := adobeLivePrivateRead(name, 32); err != nil {
		t.Fatal("private fixture rejected")
	}
	if err := adobeLivePrivateWrite(name, []byte("replacement"), true); err == nil {
		t.Fatal("exclusive replay marker overwritten")
	}
	if _, _, err := adobeLivePrivateRead(name, 2); err == nil {
		t.Fatal("oversized private input accepted")
	}
	if err := os.Chmod(name, 0644); err != nil {
		t.Fatal("fixture mode setup failed")
	}
	if _, _, err := adobeLivePrivateRead(name, 32); err == nil {
		t.Fatal("publicly readable credential input accepted")
	}
	link := filepath.Join(filepath.Dir(name), "link.json")
	if err := os.Symlink(name, link); err != nil {
		t.Fatal("fixture symlink setup failed")
	}
	if _, _, err := adobeLivePrivateRead(link, 32); err == nil {
		t.Fatal("credential symlink accepted")
	}
}

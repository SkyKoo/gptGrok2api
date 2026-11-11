package adobe

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func omittedARP(a Account) Account {
	a.MediaSessionARPMode = "omitted"
	a.MediaSessionARPSource = "spl_runtime"
	a.Cookie = ""
	return a
}
func TestMediaSessionARPKeepsLegacyStrictAndValidatesExplicitModes(t *testing.T) {
	for _, tc := range []struct {
		name, mode, source, cookie string
		valid, present             bool
	}{
		{name: "legacy present", cookie: "sherlockToken=opaque-runtime", valid: true, present: true},
		{name: "legacy missing"},
		{name: "runtime present", mode: "present", source: "spl_runtime", cookie: "sherlockToken=opaque-runtime", valid: true, present: true},
		{name: "legacy duplicates retain compatibility", cookie: "sherlockToken=first; sherlockToken=second", valid: true, present: true},
		{name: "runtime duplicate token", mode: "present", source: "spl_runtime", cookie: "sherlockToken=first; sherlockToken=second"},
		{name: "runtime malformed duplicate", mode: "present", source: "spl_runtime", cookie: "sherlockToken=first; sherlockToken"},
		{name: "runtime comma token", mode: "present", source: "spl_runtime", cookie: "sherlockToken=first,second"},
		{name: "present but missing", mode: "present", source: "spl_runtime"},
		{name: "runtime omitted", mode: "omitted", source: "spl_runtime", valid: true},
		{name: "runtime omitted with other cookies", mode: "omitted", source: "spl_runtime", cookie: "normal=session", valid: true},
		{name: "omission retains old token", mode: "omitted", source: "spl_runtime", cookie: "normal=session; sherlockToken=old"},
		{name: "omission has malformed old token", mode: "omitted", source: "spl_runtime", cookie: "sherlockToken"},
		{name: "missing source", mode: "omitted"},
		{name: "missing mode", source: "spl_runtime"},
		{name: "unknown source", mode: "omitted", source: "cookie_cache"},
		{name: "unknown mode", mode: "optional", source: "spl_runtime"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := fixtureAccount(t)
			a.Cookie = tc.cookie
			a.MediaSessionARPMode = tc.mode
			a.MediaSessionARPSource = tc.source
			value, present, err := MediaSessionARP(a)
			if (err == nil) != tc.valid || present != tc.present {
				t.Fatalf("valid=%t present=%t err=%v", err == nil, present, err)
			}
			if tc.valid && !tc.present && value != "" {
				t.Fatal("omitted ARP still returned a token")
			}
			a.ExpiresAt = time.Now().Add(time.Hour).Unix()
			if Eligible(a, time.Now()) != tc.valid {
				t.Fatal("eligibility differs from media contract")
			}
			store := NewStore(filepath.Join(t.TempDir(), "accounts.json.enc"), "fixture")
			_, err = store.Import([]Account{a})
			if (err == nil) != tc.valid {
				t.Fatal("import differs from media contract")
			}
		})
	}
}

func TestExplicitARPModeSurvivesImportAndDoesNotClaimLiveAuthentication(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "accounts.json.enc"), "fixture")
	a := omittedARP(verifiedBrowserAccount(fixtureAccount(t)))
	imported, err := store.Import([]Account{a})
	if err != nil {
		t.Fatal(err)
	}
	saved := imported[0]
	if saved.MediaSessionARPMode != "omitted" || saved.MediaSessionARPSource != "spl_runtime" || saved.MediaSessionStatus != "imported" || saved.MediaSessionRefreshed || Public(saved)["media_session_refreshed"] != false {
		t.Fatal("import lost explicit decision or claimed a fresh browser run")
	}
	restarted := NewStore(store.path, store.secret)
	saved, err = restarted.Get(saved.ID)
	if err != nil || !Eligible(saved, time.Now()) {
		t.Fatal("persisted explicit omission was lost")
	}
	// An old record with missing cookies is still incomplete after process restart.
	if err = restarted.update(saved.ID, saved.Revision, func(a *Account) { a.MediaSessionARPMode = ""; a.MediaSessionARPSource = "" }); err != nil {
		t.Fatal(err)
	}
	saved, _ = restarted.Get(saved.ID)
	if Eligible(saved, time.Now()) {
		t.Fatal("legacy record auto-upgraded to omitted")
	}
}

func TestSubmitRespectsExplicitARPDecisionWithoutEmptyHeader(t *testing.T) {
	for _, kind := range []string{"image", "video"} {
		for _, mode := range []string{"legacy", "present", "omitted"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				a := fixtureAccount(t)
				if mode == "present" {
					a.MediaSessionARPMode = "present"
					a.MediaSessionARPSource = "spl_runtime"
				}
				if mode == "omitted" {
					a = omittedARP(a)
				}
				calls := 0
				client := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
					calls++
					_, hasHeader := r.Header[http.CanonicalHeaderKey("x-arp-session-id")]
					if mode == "omitted" && hasHeader {
						t.Fatal("omitted ARP sent an empty or old header")
					}
					if mode != "omitted" && r.Header.Get("x-arp-session-id") == "" {
						t.Fatal("present ARP dropped")
					}
					if r.Header.Get("Authorization") != "Bearer "+a.Token || r.Header.Get("x-nonce") != Nonce(a.AccountID, "fixture") || r.GetBody != nil {
						t.Fatal("auth, nonce or replay contract changed")
					}
					return jsonResponse(200, `{"links":{"result":{"href":"https://bks-epo8552.adobe.io/v2/jobs/result/fixture"}}}`), nil
				})})
				var err error
				if kind == "image" {
					_, err = client.SubmitImage(context.Background(), a, ImageRequest{Model: "gemini-3.1-flash-image", Prompt: "fixture", Width: 1024, Height: 1024, N: 1})
				} else {
					_, err = client.SubmitVideo(context.Background(), a, VideoRequest{Model: "veo-3.1-lite", Prompt: "fixture", Width: 1920, Height: 1080, Duration: 8, Resolution: "1080p", AspectRatio: "16:9"})
				}
				if err != nil || calls != 1 {
					t.Fatalf("calls=%d err=%v", calls, err)
				}
			})
		}
	}
}

func TestOmittedARPAllowsReferencesButLegacyMissingStopsBeforeUpload(t *testing.T) {
	for _, omitted := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "omitted"}[omitted], func(t *testing.T) {
			a := fixtureAccount(t)
			a.Cookie = ""
			if omitted {
				a = omittedARP(a)
			}
			calls := 0
			client := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if _, present := r.Header[http.CanonicalHeaderKey("x-arp-session-id")]; present {
					t.Fatal("omitted ARP emitted a header")
				}
				if r.URL.Path == "/v2/storage/image" {
					return jsonResponse(200, `{"images":[{"id":"reference"}]}`), nil
				}
				return jsonResponse(200, `{"links":{"result":{"href":"https://bks-epo8552.adobe.io/v2/jobs/result/fixture"}}}`), nil
			})})
			_, err := client.SubmitImage(context.Background(), a, ImageRequest{Model: "gemini-3.1-flash-image", Prompt: "fixture", Width: 1024, Height: 1024, N: 1, References: []ReferenceImage{referenceFixture(t, 3, 3)}})
			if omitted && (err != nil || calls != 2) {
				t.Fatal("explicit omission rejected reference flow", err)
			}
			if !omitted && (err == nil || calls != 0) {
				t.Fatal("legacy missing ARP reached upload")
			}
		})
	}
}

func TestBrowserARPTransitionClearsOldCookieAndKeepsUnknownReservation(t *testing.T) {
	store, a := readyStore(t)
	if _, err := store.Reserve("unknown-arp-task"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkUnknown("unknown-arp-task"); err != nil {
		t.Fatal(err)
	}
	store.SetBrowserRefresher(func(ctx context.Context, current Account) (Account, error) {
		return omittedARP(verifiedBrowserAccount(current)), nil
	})
	fresh, err := store.Relogin(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Cookie != "" || fresh.MediaSessionARPMode != "omitted" || fresh.MediaSessionARPSource != "spl_runtime" || fresh.MediaSessionStatus != "refreshed" {
		t.Fatal("relogin retained old cookie or lost new runtime decision")
	}
	if fresh.InFlight != "unknown-arp-task" || fresh.Status != "submission_unknown" || fresh.LastError != "adobe_submission_unknown" {
		t.Fatal("ARP transition released unknown generation")
	}
	if _, err = store.Reserve("new-task"); !errors.Is(err, ErrNoAccount) {
		t.Fatal("ARP omission bypassed reservation")
	}
	// A later normal browser session may supply ARP again, with the same identity.
	store.SetBrowserRefresher(func(ctx context.Context, current Account) (Account, error) {
		current = verifiedBrowserAccount(current)
		current.MediaSessionARPMode = "present"
		current.Cookie = "sherlockToken=new-runtime"
		return current, nil
	})
	fresh, err = store.Relogin(context.Background(), a.ID)
	if err != nil || fresh.MediaSessionARPMode != "present" || fresh.Cookie != "sherlockToken=new-runtime" {
		t.Fatal("browser could not renew explicit present ARP", err)
	}
}

func TestIMSRefreshPreservesARPDecisionAndCannotEstablishOmission(t *testing.T) {
	store, a := readyStore(t)
	replacement := omittedARP(a)
	if _, err := store.ReplaceCredentials(a, replacement); SafeError(err) != "adobe_refresh_arp_policy_changed" {
		t.Fatal("token-only refresh established omission")
	}
	rows, err := store.Import([]Account{replacement})
	if err != nil {
		t.Fatal(err)
	}
	a = rows[0]
	a.RefreshCookie = "imsSession=fixture"
	a, err = store.ReplaceCredentials(a, a)
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/ims/check/v6/token" {
			raw, _ := json.Marshal(map[string]string{"access_token": a.Token})
			return jsonResponse(200, string(raw)), nil
		}
		if r.URL.Path == "/v1/credits/balance" {
			return jsonResponse(200, `{"total":{"quota":{"available":10}}}`), nil
		}
		t.Fatal("unexpected media request")
		return nil, nil
	})})
	fresh, err := store.Refresh(context.Background(), client, a.ID, true)
	if err != nil || fresh.MediaSessionARPMode != "omitted" || fresh.MediaSessionARPSource != "spl_runtime" || fresh.Cookie != "" || fresh.MediaSessionStatus != "preserved" {
		t.Fatal("IMS refresh changed explicit runtime decision", err)
	}
	candidate := fresh
	candidate.Cookie = "sherlockToken=historical"
	fresh, err = store.ReplaceCredentials(fresh, candidate)
	if err != nil || fresh.Cookie != "" || fresh.MediaSessionARPMode != "omitted" {
		t.Fatal("IMS refresh resurrected stale cookie", err)
	}
}

func TestBrowserFallbackRefreshAndPinnedTaskAcceptExplicitOmission(t *testing.T) {
	for _, taskMode := range []bool{false, true} {
		t.Run(map[bool]string{false: "maintenance", true: "pinned task"}[taskMode], func(t *testing.T) {
			store, a := readyStore(t)
			a.ProfileID = "fixture-profile"
			rows, err := store.Import([]Account{a})
			if err != nil {
				t.Fatal(err)
			}
			a = rows[0]
			if taskMode {
				if _, err = store.Reserve("pinned-arp-task"); err != nil {
					t.Fatal(err)
				}
				if err = store.SaveJob("pinned-arp-task", Job{Kind: "video", PollURL: "https://bks-epo8552.adobe.io/v2/jobs/result/pinned"}); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			store.SetBrowserRefresher(func(ctx context.Context, current Account) (Account, error) {
				calls++
				return omittedARP(verifiedBrowserAccount(current)), nil
			})
			client := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/v1/credits/balance" {
					t.Fatal("fallback submitted media")
				}
				return jsonResponse(200, `{"total":{"quota":{"available":10}}}`), nil
			})})
			var fresh Account
			if taskMode {
				fresh, err = store.RefreshTask(context.Background(), client, "pinned-arp-task", a.ID)
			} else {
				fresh, err = store.Refresh(context.Background(), client, a.ID, true)
			}
			if err != nil || calls != 1 || fresh.Cookie != "" || fresh.MediaSessionARPMode != "omitted" || fresh.MediaSessionStatus != "refreshed" {
				t.Fatal("browser fallback lost explicit omission", err)
			}
			if taskMode && fresh.InFlight != "pinned-arp-task" {
				t.Fatal("browser fallback lost pinned task")
			}
		})
	}
}

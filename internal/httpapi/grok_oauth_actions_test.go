package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/oauth"
)

func TestOAuthAccountMutationsCannotRaceQualityAutoDelivery(t *testing.T) {
	for _, action := range []string{"disable", "enable", "delete"} {
		t.Run(action, func(t *testing.T) {
			s := New(adminTestConfig(t.TempDir()))
			remote := installXAITestCPA(t, s)
			item, err := s.oauthStore.Import(oauth.Account{Email: "test@example.test", AccessToken: "local", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), CPASync: &oauth.CPASync{Status: "pending", PoolID: "test-cpa"}})
			if err != nil {
				t.Fatal(err)
			}
			idleStatus := "active"
			if action == "enable" {
				idleStatus = "disabled"
			}
			idle, err := s.oauthStore.Import(oauth.Account{Email: "idle@example.test", AccessToken: "idle", Status: idleStatus})
			if err != nil {
				t.Fatal(err)
			}
			listed, resume := make(chan struct{}), make(chan struct{})
			var listOnce, resumeOnce sync.Once
			release := func() { resumeOnce.Do(func() { close(resume) }) }
			defer release()
			remote.beforeList = func() {
				listOnce.Do(func() {
					close(listed)
					<-resume
				})
			}
			delivered := make(chan error, 1)
			go func() {
				_, err := s.checkGrokQuality(context.Background(), item.ID, false)
				delivered <- err
			}()
			select {
			case <-listed:
			case <-time.After(5 * time.Second):
				t.Fatal("quality did not reach CPA delivery preflight")
			}
			method, path := http.MethodPost, "/api/grok/oauth/accounts/status"
			if action == "delete" {
				method, path = http.MethodDelete, "/api/grok/oauth/accounts"
			}
			// Put the idle row first, repeat it, and pad the busy ID. A conflict
			// must neither change earlier rows nor leak their acquired locks.
			body, _ := json.Marshal(map[string]any{"ids": []string{idle.ID, idle.ID, " " + item.ID + " "}, "disabled": action == "disable"})
			response := adminRequest(s.Handler(), method, path, strings.NewReader(string(body)))
			if response.Code != http.StatusConflict {
				t.Fatalf("mutation accepted during upload preflight: status=%d body=%s", response.Code, response.Body.String())
			}
			for _, original := range []oauth.Account{idle, item} {
				current, found, err := s.oauthStore.Get(original.ID)
				if err != nil || !found || current.Status != original.Status || !oauth.SameCredential(current, original) || !reflect.DeepEqual(current.CPASync, original.CPASync) {
					t.Fatalf("rejected batch changed account %s", original.ID)
				}
			}
			if _, held := s.grokOAuthActions.Load(idle.ID); held {
				t.Fatal("failed batch leaked the idle account lock")
			}
			if _, held := s.grokOAuthActions.Load(item.ID); !held {
				t.Fatal("failed batch released another operation's lock")
			}
			release()
			select {
			case err := <-delivered:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("delivery did not complete")
			}
			if remote.uploads != 1 {
				t.Fatalf("delivery attempts=%d, want 1", remote.uploads)
			}
			response = adminRequest(s.Handler(), method, path, strings.NewReader(string(body)))
			if response.Code != http.StatusOK {
				t.Fatalf("mutation remained blocked after delivery: %d %s", response.Code, response.Body.String())
			}
			for _, id := range []string{idle.ID, item.ID} {
				current, found, err := s.oauthStore.Get(id)
				if err != nil || (action == "delete" && found) || (action != "delete" && (!found || (current.Status == "disabled") != (action == "disable"))) {
					t.Fatalf("successful mutation was not applied: action=%s account=%s", action, id)
				}
				if _, held := s.grokOAuthActions.Load(id); held {
					t.Fatal("successful mutation leaked its lock")
				}
			}
			if action != "enable" {
				if _, err := s.checkGrokQuality(context.Background(), item.ID, false); err == nil || remote.uploads != 1 {
					t.Fatal("quality restarted after successful disable/delete")
				}
			}
		})
	}
}

func TestOAuthAccountMutationBatchPreservesStatusAndCountContract(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	var ids []string
	for _, status := range []string{"disabled", "invalid", "expired"} {
		item, err := s.oauthStore.Import(oauth.Account{Email: status + "@example.test", AccessToken: status, Status: status})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, item.ID)
	}
	requestIDs := append(append([]string{}, ids...), " "+ids[0]+" ", "", "missing")
	for _, action := range []string{"enable", "disable", "delete"} {
		method, path, countField := http.MethodPost, "/api/grok/oauth/accounts/status", "updated"
		if action == "delete" {
			method, path, countField = http.MethodDelete, "/api/grok/oauth/accounts", "removed"
		}
		body, _ := json.Marshal(map[string]any{"ids": requestIDs, "disabled": action == "disable"})
		response := adminRequest(s.Handler(), method, path, strings.NewReader(string(body)))
		var result map[string]any
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || result[countField] != float64(3) {
			t.Fatalf("batch contract changed: %d %s", response.Code, response.Body.String())
		}
		if action != "delete" && result["disabled"] != (action == "disable") {
			t.Fatal("status response lost disabled flag")
		}
		for _, id := range ids {
			item, found, err := s.oauthStore.Get(id)
			wantStatus := "active"
			if action == "disable" {
				wantStatus = "disabled"
			}
			if err != nil || (action == "delete" && found) || (action != "delete" && (!found || item.Status != wantStatus)) {
				t.Fatalf("existing %s semantics changed", action)
			}
		}
	}
}

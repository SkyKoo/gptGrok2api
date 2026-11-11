package httpapi

import (
	"context"
	"encoding/json"
	"github.com/auucoder/gptgrok2api-go/internal/adobe"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdobeAccountContextOmitsAbsentBinding(t *testing.T) {
	s := imageTaskTestServer(t)
	s.proxyManager.SetDefault("http://default.invalid:8080", nil)
	a := adobe.Account{Proxy: "direct"}
	ctx, err := s.adobeAccountContext(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, present := proxyruntime.URLSelectionFromContext(ctx)
	if !present || endpoint != "" {
		t.Fatal("explicit direct account must bypass the default")
	}
	continued, err := s.adobeAccountContext(ctx, adobe.Account{})
	if selected, ok := proxyruntime.URLSelectionFromContext(continued); err != nil || !ok || selected != "" {
		t.Fatal("an active direct flow switched to the default proxy")
	}
	a.ProxyBinding = &proxyruntime.Binding{Reference: "node:missing/node", Platform: "adobe", Purpose: "account"}
	if _, err = s.adobeAccountContext(context.Background(), a); err == nil {
		t.Fatal("missing bound proxy silently changed egress")
	}
}

func TestAdobeUnconfiguredAccountFollowsDefaultAcrossUpdates(t *testing.T) {
	s := imageTaskTestServer(t)
	a := adobe.Account{}
	for _, endpoint := range []string{"http://default-one.invalid:8080", "socks5://default-two.invalid:1080"} {
		s.proxyManager.SetDefault(endpoint, nil)
		ctx, err := s.adobeAccountContext(context.Background(), a)
		if err != nil {
			t.Fatal(err)
		}
		actual, selected := proxyruntime.URLSelectionFromContext(ctx)
		if !selected || actual != endpoint {
			t.Fatal("unconfigured Adobe account did not follow the current default")
		}
	}
	a.Proxy = "http://account.invalid:8080"
	a.ProxyPolicy = "explicit"
	ctx, err := s.adobeAccountContext(context.Background(), a)
	if err != nil || proxyruntime.URLFromContext(ctx) != a.Proxy {
		t.Fatal("explicit Adobe override lost priority")
	}
}

func TestAdobeRegistrationRecoverySeparatesDailyProxy(t *testing.T) {
	source := imageTaskTestServer(t)
	fixture := adobeFixtureAccount(t, source)
	raw, _ := json.Marshal(fixture)
	var fields map[string]any
	_ = json.Unmarshal(raw, &fields)
	fields["proxy"] = "http://signup.invalid:8080"
	fields["proxy_binding"] = map[string]any{"reference": "http://signup.invalid:8080", "purpose": "registration", "platform": "adobe"}
	s := imageTaskTestServer(t)
	if err := s.importRegisteredAdobeAccount(context.Background(), "adobe-registration", fields); err != nil {
		t.Fatal(err)
	}
	a, err := s.adobeStore.Get(fixture.ID)
	if err != nil || a.ProxyPolicy != "inherit" || a.Proxy != "" || a.ProxyBinding != nil || fields["proxy_binding"] == nil {
		t.Fatal("Adobe import copied the registration route or changed the saved result")
	}
	_, err = s.adobeStore.BindProxy(a.ID, proxyruntime.Binding{Reference: "direct", Platform: "adobe", Purpose: "account"}, "manual daily direct")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.importRegisteredAdobeAccount(context.Background(), "adobe-registration", fields); err != nil {
		t.Fatal(err)
	}
	a, _ = s.adobeStore.Get(fixture.ID)
	if a.ProxyPolicy != "explicit" || a.Proxy != "direct" || len(a.ProxyBindingHistory) != 1 {
		t.Fatal("registration recovery replaced a manual Adobe override")
	}
}

func TestAdobeBrowserRefreshUsesDailyDefaultAndPinnedFlow(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		t.Run(map[bool]string{false: "current_default", true: "existing_flow"}[pinned], func(t *testing.T) {
			s := NewWithWorkers(adminTestConfig(t.TempDir()), false)
			a := adobeFixtureAccount(t, s)
			a.ProfileID = "saved-profile"
			want := "http://current-default.invalid:8080"
			s.proxyManager.SetDefault(want, nil)
			ctx := context.Background()
			if pinned {
				want = "http://started-flow.invalid:8080"
				ctx = proxyruntime.WithURL(ctx, want)
			}
			worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					w.Write([]byte(`{"status":"released"}`))
					return
				}
				switch r.URL.Path {
				case "/v1/browser/reservations":
					var body map[string]any
					if json.NewDecoder(r.Body).Decode(&body) != nil || body["proxy"] != want {
						t.Error("Adobe refresh browser did not use the daily flow route")
					}
					w.Write([]byte(`{"status":"ready"}`))
				case "/v1/adobe/run":
					_ = json.NewEncoder(w).Encode(map[string]any{"stage": "completed", "account": a})
				default:
					t.Error("unexpected browser request")
					w.WriteHeader(404)
				}
			}))
			defer worker.Close()
			if _, err := s.registerStore.UpdateForTarget("adobe", map[string]any{"adobe": map[string]any{"api_base": worker.URL}}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.refreshAdobeBrowserAccount(ctx, a); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestAdobeAccountPublicListAndAdminIsolation(t *testing.T) {
	s := imageTaskTestServer(t)
	a := adobeFixtureAccount(t, s)
	unauthorized := imageTaskCall(s, "GET", "/api/adobe/accounts", "api-secret", "")
	if unauthorized.Code != 401 && unauthorized.Code != 403 {
		t.Fatalf("public client accessed admin list: %d", unauthorized.Code)
	}
	response := imageTaskCall(s, "GET", "/api/adobe/accounts", "admin-secret", "")
	if response.Code != 200 {
		t.Fatalf("list: %d %s", response.Code, response.Body.String())
	}
	for _, secret := range []string{a.Token, a.Cookie, "sherlockToken"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatal("credentials in account list")
		}
	}
	var out struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	if json.Unmarshal(response.Body.Bytes(), &out) != nil || out.Total != 1 {
		t.Fatal("account list contract invalid")
	}
}

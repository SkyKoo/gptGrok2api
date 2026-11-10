package httpapi

import (
	"context"
	"encoding/json"
	"github.com/auucoder/gptgrok2api-go/internal/adobe"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
	"strings"
	"testing"
)

func TestAdobeAccountContextOmitsAbsentBinding(t *testing.T) {
	s := imageTaskTestServer(t)
	a := adobe.Account{Proxy: "direct"}
	ctx, err := s.adobeAccountContext(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, present := proxyruntime.URLSelectionFromContext(ctx)
	if !present || endpoint != "" {
		t.Fatal("unbound import must use explicit direct egress")
	}
	a.ProxyBinding = &proxyruntime.Binding{Reference: "node:missing/node", Platform: "adobe", Purpose: "account"}
	if _, err = s.adobeAccountContext(context.Background(), a); err == nil {
		t.Fatal("missing bound proxy silently changed egress")
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

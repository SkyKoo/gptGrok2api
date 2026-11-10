package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestAdobeRegistrationAPITargetAndSecretIsolation(t *testing.T) {
	s := scheduleServer(t)
	handler := s.Handler()
	response := adminRequest(handler, http.MethodPost, "/api/register?target=adobe", strings.NewReader(`{"total":7,"threads":1,"adobe":{"api_base":"http://worker.example.test:8899","api_key":"PRIVATE-ADOBE-WORKER","country":"US","birth_year":1980,"birth_month":10}}`))
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	if strings.Contains(response.Body.String(), "PRIVATE-ADOBE-WORKER") {
		t.Fatal("worker key exposed")
	}
	if intValue(s.registerStore.ConfigForTarget("adobe")["total"]) != 7 || intValue(s.registerStore.ConfigForTarget("openai")["total"]) != 3 {
		t.Fatal("registration target config crossed")
	}
	if s.registerStore.Get()["target"] != "openai" {
		t.Fatal("tab changed legacy target")
	}
	response = adminRequest(handler, http.MethodPost, "/api/register?target=adobe", strings.NewReader(`{"adobe":{"api_base":"http://worker.example.test:8899","api_key":""}}`))
	if response.Code != 200 || stringValue(mapValue(s.registerStore.Get()["adobe"])["api_key"]) != "PRIVATE-ADOBE-WORKER" {
		t.Fatal("blank key did not retain same endpoint secret")
	}
	response = adminRequest(handler, http.MethodGet, "/api/register/runtime?target=adobe", nil)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	var body map[string]any
	if json.Unmarshal(response.Body.Bytes(), &body) != nil || mapValue(body["runtime"])["target"] != "adobe" {
		t.Fatal("runtime ignored explicit Adobe target")
	}
	response = adminRequest(handler, http.MethodGet, "/api/register/adobe/jobs", nil)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	response = adminRequest(handler, http.MethodGet, "/api/register/schedule?target=adobe", nil)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"target":"adobe"`) {
		t.Fatal("Adobe schedule missing")
	}
	response = adminRequest(handler, http.MethodPost, "/api/register?target=adobe", strings.NewReader(`{"adobe":{"api_base":"http://different.example.test:8899","api_key":""}}`))
	if response.Code != 200 || stringValue(mapValue(s.registerStore.Get()["adobe"])["api_key"]) != "" {
		t.Fatal("worker key forwarded to new endpoint")
	}
}

func TestAdobeMailboxCleanupNeverDefaultsToOpenAI(t *testing.T) {
	for _, item := range []map[string]any{{"platform": "adobe"}, {"source_type": "adobe_browser"}, {"registration_mailbox": map[string]any{"platform": "adobe"}}} {
		if mailboxPlatform(item) != "adobe" {
			t.Fatal("Adobe cleanup selected another platform")
		}
	}
}

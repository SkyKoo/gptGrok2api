package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAccountBrowserCookiesArePrivate(t *testing.T) {
	row := map[string]any{"access_token": "token", "web_cookies_token_hash": "private-cookie-binding", "web_cookies": []any{map[string]any{"name": "session", "value": "private-cookie"}}, "cookie": "private-legacy", "cookies": "private-other"}
	raw, _ := json.Marshal(accountForAPI(row))
	for _, secret := range []string{"private-cookie", "private-cookie-binding", "private-legacy", "private-other", "web_cookies"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("private cookie appeared in account API")
		}
	}
	if row["web_cookies"] == nil {
		t.Fatal("redaction removed stored cookies")
	}
}

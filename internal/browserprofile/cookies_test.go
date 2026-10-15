package browserprofile

import (
	"encoding/json"
	"net/url"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
)

func TestCookieSnapshotPreservesScopeAndExpiry(t *testing.T) {
	root, _ := url.Parse("https://chatgpt.com/")
	auth, _ := url.Parse("https://auth.openai.com/")
	jar := NewCookieJar(root.String())
	jar.SetCookies(root, []*fhttp.Cookie{
		{Name: "session", Value: "private", Path: "/", Secure: true, HttpOnly: true, MaxAge: 60},
		{Name: "same", Value: "root", Path: "/"},
		{Name: "same", Value: "backend", Path: "/backend-api"},
		{Name: "expired", Value: "old", Path: "/", Expires: time.Now().Add(-time.Second)},
		{Name: "invalid", Value: "cross-domain", Domain: "openai.com", Path: "/"},
	})
	jar.SetCookies(auth, []*fhttp.Cookie{{Name: "auth-only", Value: "secret", Path: "/"}})
	snapshot := jar.Snapshot()
	if len(snapshot) != 3 {
		t.Fatalf("wrong cookie count: %d", len(snapshot))
	}
	for _, c := range snapshot {
		if c.Name == "session" && (c.Expires.Before(time.Now().Add(50*time.Second)) || !c.HTTPOnly || !c.Secure) {
			t.Fatal("lost cookie attributes")
		}
	}
	raw, _ := json.Marshal(snapshot)
	var persisted any
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	restored := NewCookieJar(root.String())
	restored.Restore(persisted)
	for _, tc := range []struct {
		raw  string
		want int
	}{
		{"https://chatgpt.com/", 2}, {"https://chatgpt.com/backend-api/me", 3},
		{"http://chatgpt.com/", 1}, {"https://auth.openai.com/", 0},
		{"https://chatgpt.com.evil.test/", 0}, {"https://sub.chatgpt.com/", 0},
	} {
		u, _ := url.Parse(tc.raw)
		if got := len(restored.Cookies(u)); got != tc.want {
			t.Errorf("%s: got %d cookies, want %d", tc.raw, got, tc.want)
		}
	}
	restored.SetCookies(root, []*fhttp.Cookie{{Name: "session", Path: "/", MaxAge: -1}})
	if len(restored.Snapshot()) != 2 {
		t.Fatal("deleted cookie resurrected")
	}
	copy := restored.Snapshot()
	copy[0].Value = "changed"
	if restored.Snapshot()[0].Value == "changed" {
		t.Fatal("snapshot shares mutable state")
	}
}

func TestRestoreRejectsForeignExpiredAndInvalidCookies(t *testing.T) {
	jar := NewCookieJar("https://chatgpt.com")
	jar.Restore([]Cookie{
		{Name: "foreign", Value: "secret", Domain: "evil.test", Path: "/"},
		{Name: "expired", Value: "secret", Path: "/", Expires: time.Now().Add(-time.Hour)},
		{Name: "bad name", Value: "secret", Path: "/"},
	})
	u, _ := url.Parse("https://chatgpt.com/")
	if len(jar.Snapshot()) != 0 || len(jar.Cookies(u)) != 0 {
		t.Fatal("invalid saved cookies were restored")
	}
}

func TestCookieSnapshotTracksDomainReplacement(t *testing.T) {
	u, _ := url.Parse("https://chatgpt.com/")
	jar := NewCookieJar(u.String())
	jar.SetCookies(u, []*fhttp.Cookie{{Name: "session", Value: "old", Path: "/"}})
	jar.SetCookies(u, []*fhttp.Cookie{{Name: "session", Value: "new", Path: "/", Domain: ".chatgpt.com"}})
	if got := jar.Snapshot(); len(got) != 1 || got[0].Value != "new" {
		t.Fatal("snapshot retained replaced cookie")
	}
	jar.SetCookies(u, []*fhttp.Cookie{{Name: "session", Path: "/", MaxAge: -1}})
	if len(jar.Snapshot()) != 0 || len(jar.Cookies(u)) != 0 {
		t.Fatal("deleted cookie can be restored")
	}
}

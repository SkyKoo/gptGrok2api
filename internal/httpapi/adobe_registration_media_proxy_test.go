package httpapi

import (
	"context"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

func TestAdobeRegistrationMediaUsesCurrentDailyDefault(t *testing.T) {
	s := imageTaskTestServer(t)
	for _, route := range []string{"http://daily-one.invalid:8080", "socks5://daily-two.invalid:1080", ""} {
		s.proxyManager.SetDefault(route, nil)
		got, err := s.adobeRegistrationMediaProxy(context.Background(), "new@example.test", "new-profile")
		if err != nil || got != route {
			t.Fatalf("daily default mismatch: %v", err)
		}
	}
}

func TestAdobeRegistrationMediaPreservesOverrideOnlyForSameProfile(t *testing.T) {
	s := imageTaskTestServer(t)
	a := adobeFixtureAccount(t, s)
	a.ProfileID = "existing-profile"
	if _, err := s.adobeStore.Import([]adobe.Account{a}); err != nil {
		t.Fatal(err)
	}
	s.proxyManager.SetDefault("http://default.invalid:8080", nil)
	for _, reference := range []string{"direct", "http://manual.invalid:8080"} {
		if _, err := s.adobeStore.BindProxy(a.ID, proxyruntime.Binding{Reference: reference, Platform: "adobe", Purpose: "account"}, "test route"); err != nil {
			t.Fatal(err)
		}
		got, err := s.adobeRegistrationMediaProxy(context.Background(), a.Email, a.ProfileID)
		want := reference
		if reference == "direct" {
			want = ""
		}
		if err != nil || got != want {
			t.Fatal("existing explicit route was replaced")
		}
		if _, err := s.adobeRegistrationMediaProxy(context.Background(), a.Email, "another-profile"); err == nil {
			t.Fatal("another profile inherited account ownership")
		}
	}
	if _, err := s.adobeStore.BindProxy(a.ID, proxyruntime.Binding{Reference: "node:missing/node", Platform: "adobe", Purpose: "account"}, "missing route"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.adobeRegistrationMediaProxy(context.Background(), a.Email, a.ProfileID); err == nil {
		t.Fatal("unavailable override fell through to default/direct")
	}
}

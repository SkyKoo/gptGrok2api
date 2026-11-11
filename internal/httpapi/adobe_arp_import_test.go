package httpapi

import (
	"context"
	"encoding/json"
	"testing"
)

func TestAdobeManualAndRegistrationImportPreserveExplicitARPMode(t *testing.T) {
	for _, registration := range []bool{false, true} {
		t.Run(map[bool]string{false: "manual", true: "registration"}[registration], func(t *testing.T) {
			s := imageTaskTestServer(t)
			account := adobeFixtureAccount(t, s)
			if _, err := s.adobeStore.Delete([]string{account.ID}); err != nil {
				t.Fatal(err)
			}
			account.Cookie = ""
			account.MediaSessionARPMode = "omitted"
			account.MediaSessionARPSource = "spl_runtime"
			account.MediaSessionRefreshed = true
			account.MediaSessionStatus = "complete"
			if registration {
				raw, _ := json.Marshal(account)
				var fields map[string]any
				_ = json.Unmarshal(raw, &fields)
				if err := s.importRegisteredAdobeAccount(context.Background(), "registration-arp-fixture", fields); err != nil {
					t.Fatal(err)
				}
			} else {
				raw, _ := json.Marshal(map[string]any{"accounts": []any{account}})
				response := imageTaskCall(s, "POST", "/api/adobe/accounts/import", "admin-secret", string(raw))
				if response.Code != 200 {
					t.Fatal("explicit administrator import failed", response.Body.String())
				}
			}
			saved, err := s.adobeStore.Get(account.ID)
			if err != nil || saved.MediaSessionARPMode != "omitted" || saved.MediaSessionARPSource != "spl_runtime" || saved.Cookie != "" || saved.MediaSessionStatus != "imported" || saved.MediaSessionRefreshed {
				t.Fatal("import lost marker or misreported live browser validation", err)
			}
			if registration && (saved.RegistrationJobID != "registration-arp-fixture" || saved.Enabled) {
				t.Fatal("registration bypassed post-import verification")
			}
		})
	}
}

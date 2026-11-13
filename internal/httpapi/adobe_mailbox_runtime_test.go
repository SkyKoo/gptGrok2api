package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

func TestAdobeMailboxCleanupErrorIsRuntimeOnly(t *testing.T) {
	s := scheduleServer(t)
	writeAdobeMailboxHistory(t, s, registerruntime.RegistrationJob{ID: "failed", Status: "registration_pending", Mailbox: registerruntime.Mailbox{Email: "failed@example.test"}})
	s.initAdobeRegistration()
	s.adobeRegister.SetMailboxReservationHandlers(nil, func(context.Context, registerruntime.RegistrationJob) error { return errors.New("private failure") })
	if err := s.adobeRegister.ReconcileMailboxReservations(context.Background()); err == nil {
		t.Fatal("expected cleanup failure")
	}
	response := adminRequest(s.Handler(), http.MethodGet, "/api/register?target=adobe", nil)
	var result map[string]any
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil {
		t.Fatal(response.Code)
	}
	value := mapValue(result["register"])
	if value["mailbox_cleanup_error"] != "mailbox: reservation_release_failed" || mapValue(mapValue(value["target_runtime"])["adobe"])["mailbox_cleanup_error"] != "mailbox: reservation_release_failed" {
		t.Fatal("cleanup error missing from selected target or target_runtime")
	}
	response = adminRequest(s.Handler(), http.MethodPost, "/api/register?target=adobe", strings.NewReader(`{"mailbox_cleanup_error":"injected-runtime","total":1}`))
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	raw, _ := json.Marshal(s.registerStore.Get())
	if strings.Contains(string(raw), "mailbox_cleanup_error") || strings.Contains(string(raw), "injected-runtime") {
		t.Fatal("runtime warning persisted as configuration")
	}
}

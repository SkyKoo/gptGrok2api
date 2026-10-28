package httpapi

import (
	"context"
	"strings"
	"time"
)

// Repair only claims backed by an explicit account deletion record. Unlinked
// upstream aliases and in-flight registrations are not evidence of deletion.
func (s *Server) reconcileDeletedMailboxClaims(ctx context.Context) (int, error) {
	accounts, err := s.store.AccountList()
	if err != nil {
		return 0, err
	}
	current := map[string]bool{}
	for _, account := range accounts {
		email := strings.ToLower(strings.TrimSpace(firstNonEmpty(stringValue(account["email"]), stringValue(mapValue(account["registration_mailbox"])["email"]))))
		current[email] = true
	}
	released := 0
	for page := 1; ; page++ {
		events, total, err := s.store.ListAccountHistory("deleted", "", page, 500)
		if err != nil {
			return released, err
		}
		for _, event := range events {
			email := strings.ToLower(strings.TrimSpace(stringValue(event["email"])))
			// This history belongs to the GPT account store. Grok has its own archive.
			if email == "" || current[email] || mailboxPlatform(event) != "openai" {
				continue
			}
			deletedAt, err := time.Parse(time.RFC3339Nano, stringValue(event["event_at"]))
			if err != nil {
				continue
			}
			boxes, err := s.mailboxStore.ReleaseDeletedAccount(ctx, "openai", stringValue(event["account_ref"]), stringValue(event["registration_job_id"]), email, deletedAt)
			if err != nil {
				return released, err
			}
			released += len(boxes)
		}
		if page*500 >= total {
			break
		}
	}
	return released, nil
}

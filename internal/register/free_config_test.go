package register

import "testing"

func TestParseFreeConfigUsesSelectedMailboxSource(t *testing.T) {
	raw := map[string]any{
		"target":  "openai",
		"mode":    "total",
		"total":   1,
		"threads": 1,
		"mail": map[string]any{
			"source_id": "hme-primary",
			"providers": []any{
				map[string]any{
					"id":             "hme-secondary",
					"type":           "icloud_hme",
					"enable":         true,
					"api_base":       "https://secondary.example.test",
					"admin_password": "secondary-secret",
					"account_id":     "secondary-account",
				},
				map[string]any{
					"id":             "hme-primary",
					"type":           "icloud_hme",
					"enable":         true,
					"api_base":       "https://primary.example.test",
					"admin_password": "primary-secret",
					"account_id":     "primary-account",
				},
			},
		},
	}
	cfg, err := ParseFreeConfig(raw)
	if err != nil {
		t.Fatalf("ParseFreeConfig() error = %v", err)
	}
	if cfg.MailboxSourceID != "hme-primary" || cfg.HME.SourceID != "hme-primary" {
		t.Fatalf("selected mailbox source was not retained: source=%q hme=%+v", cfg.MailboxSourceID, cfg.HME)
	}
	if cfg.HME.BaseURL != "https://primary.example.test" || cfg.HME.AccountID != "primary-account" {
		t.Fatalf("selected source config was not used: %+v", cfg.HME)
	}
}

func TestParseFreeConfigRejectsUnavailableMailboxSource(t *testing.T) {
	raw := map[string]any{
		"target":  "openai",
		"mode":    "total",
		"total":   1,
		"threads": 1,
		"mail": map[string]any{
			"source_id": "missing-source",
			"providers": []any{map[string]any{
				"id":             "hme-primary",
				"type":           "icloud_hme",
				"enable":         true,
				"api_base":       "https://primary.example.test",
				"admin_password": "primary-secret",
				"account_id":     "primary-account",
			}},
		},
	}
	if _, err := ParseFreeConfig(raw); err == nil || err.Error() != "config: selected_mailbox_source_unavailable" {
		t.Fatalf("unexpected error for unavailable source: %v", err)
	}
}

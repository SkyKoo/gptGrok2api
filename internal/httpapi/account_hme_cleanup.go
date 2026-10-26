package httpapi

import (
	"context"
	"errors"
	"strings"
	"time"

	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

// hmeCleanupResult describes the second step of account deletion. It is kept
// deliberately small and contains no HME credentials or session details.
type hmeCleanupResult struct {
	AccountRef string `json:"account_ref"`
	Email      string `json:"email,omitempty"`
	Status     string `json:"status"`
	Message    string `json:"message,omitempty"`
}

type hmeCleanupSummary struct {
	Enabled  bool               `json:"enabled"`
	Deleted  int                `json:"deleted"`
	Skipped  int                `json:"skipped"`
	Retained int                `json:"retained"`
	NotFound int                `json:"not_found"`
	Failed   int                `json:"failed"`
	Results  []hmeCleanupResult `json:"results,omitempty"`
}

func (s *Server) hmeCleanupConfig() (registerruntime.HMEConfig, bool) {
	mail := mapValue(s.registerStore.Get()["mail"])
	providers, _ := mail["providers"].([]any)
	for _, raw := range providers {
		provider := mapValue(raw)
		if stringValue(provider["type"]) != "icloud_hme" || !boolValue(provider["enable"], true) {
			continue
		}
		if !boolValue(provider["delete_hme_mailbox_on_account_delete"], false) {
			return registerruntime.HMEConfig{}, false
		}
		return registerruntime.HMEConfig{
			BaseURL:   strings.TrimRight(stringValue(provider["api_base"]), "/"),
			Password:  stringValue(provider["admin_password"]),
			AccountID: stringValue(provider["account_id"]),
		}, true
	}
	return registerruntime.HMEConfig{}, false
}

func hmeCleanupMessage(err error) string {
	var failure *registerruntime.Failure
	if errors.As(err, &failure) {
		return failure.Error()
	}
	return "HME 邮箱删除失败"
}

func isHMEErrorCode(err error, code string) bool {
	var failure *registerruntime.Failure
	return errors.As(err, &failure) && failure.Code == code
}

func hmeMailboxFromAccount(account map[string]any) (registerruntime.Mailbox, bool) {
	mailbox := mapValue(account["registration_mailbox"])
	if !strings.EqualFold(stringValue(mailbox["provider"]), "icloud_hme") {
		return registerruntime.Mailbox{}, false
	}
	return registerruntime.Mailbox{
		ID:              strings.TrimSpace(stringValue(mailbox["id"])),
		Email:           strings.TrimSpace(stringValue(mailbox["email"])),
		AccountID:       strings.TrimSpace(stringValue(mailbox["account_id"])),
		SourceType:      firstNonEmpty(stringValue(mailbox["source_type"]), "hme"),
		SourceMailboxID: strings.TrimSpace(stringValue(mailbox["source_mailbox_id"])),
	}, true
}

func mailboxPlatform(account map[string]any) string {
	mailbox := mapValue(account["registration_mailbox"])
	for _, key := range []string{"platform", "source_type", "type"} {
		value := strings.ToLower(strings.TrimSpace(stringValue(mailbox[key])))
		if value == "grok" || value == "xai" {
			return "grok"
		}
	}
	for _, key := range []string{"platform", "source_type", "type"} {
		value := strings.ToLower(strings.TrimSpace(stringValue(account[key])))
		if value == "grok" || value == "xai" || strings.Contains(value, "grok") {
			return "grok"
		}
	}
	return "openai"
}

func (s *Server) cleanupHMEMailboxes(ctx context.Context, accounts []map[string]any) hmeCleanupSummary {
	cfg, enabled := s.hmeCleanupConfig()
	summary := hmeCleanupSummary{Enabled: enabled}
	if !enabled {
		return summary
	}
	summary.Results = make([]hmeCleanupResult, 0, len(accounts))
	if len(accounts) == 0 {
		return summary
	}
	if strings.TrimSpace(s.cfg.MailboxDatabaseURL) != "" {
		if s.mailboxStore == nil {
			message := "邮箱资产数据库不可用，已保留上游邮箱"
			if s.mailboxInitErr == nil {
				message = "邮箱资产数据库未配置，已保留上游邮箱"
			}
			for _, account := range accounts {
				box, isHME := hmeMailboxFromAccount(account)
				result := hmeCleanupResult{AccountRef: accountPublicRef(account), Email: box.Email}
				if !isHME {
					result.Status = "not_hme_source"
					summary.Skipped++
				} else {
					result.Status = "retained"
					result.Message = message
					summary.Retained++
				}
				summary.Results = append(summary.Results, result)
			}
			return summary
		}
		return s.cleanupCoordinatedMailboxes(ctx, cfg, accounts, summary)
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()

	hme, err := registerruntime.NewHME(cfg, time.Second)
	if err == nil {
		defer hme.Close()
	}
	for _, account := range accounts {
		result := hmeCleanupResult{AccountRef: accountPublicRef(account)}
		box, isHME := hmeMailboxFromAccount(account)
		result.Email = box.Email
		if !isHME {
			result.Status = "not_hme_source"
			summary.Skipped++
			summary.Results = append(summary.Results, result)
			continue
		}
		if err != nil {
			result.Status = "failed"
			result.Message = hmeCleanupMessage(err)
			summary.Failed++
			summary.Results = append(summary.Results, result)
			continue
		}
		if cleanupErr := hme.DeleteMailbox(cleanupCtx, box); cleanupErr != nil {
			if isHMEErrorCode(cleanupErr, "mailbox_not_found") {
				result.Status = "not_found"
				summary.NotFound++
			} else {
				result.Status = "failed"
				result.Message = hmeCleanupMessage(cleanupErr)
				summary.Failed++
			}
		} else {
			result.Status = "deleted"
			summary.Deleted++
		}
		summary.Results = append(summary.Results, result)
	}
	return summary
}

func (s *Server) cleanupCoordinatedMailboxes(ctx context.Context, cfg registerruntime.HMEConfig, accounts []map[string]any, summary hmeCleanupSummary) hmeCleanupSummary {
	cleanupCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	hme, err := registerruntime.NewHME(cfg, time.Second)
	if err == nil {
		defer hme.Close()
	}
	for _, account := range accounts {
		result := hmeCleanupResult{AccountRef: accountPublicRef(account)}
		box, isHME := hmeMailboxFromAccount(account)
		result.Email = box.Email
		if !isHME {
			result.Status = "not_hme_source"
			summary.Skipped++
			summary.Results = append(summary.Results, result)
			continue
		}
		if err != nil {
			result.Status = "failed"
			result.Message = hmeCleanupMessage(err)
			summary.Failed++
			summary.Results = append(summary.Results, result)
			continue
		}
		platform := mailboxPlatform(account)
		accountID := firstNonEmpty(stringValue(account["id"]), stringValue(account["account_id"]), result.AccountRef)
		candidates, releaseErr := s.mailboxStore.ReleaseForAccount(cleanupCtx, platform, accountID, box.Email)
		if releaseErr != nil {
			result.Status = "failed"
			result.Message = releaseErr.Error()
			summary.Failed++
			summary.Results = append(summary.Results, result)
			continue
		}
		if len(candidates) == 0 {
			// The registry has no claim for this account. Retain the upstream
			// alias instead of risking deletion of an asset owned by another
			// platform or an un-migrated legacy account.
			result.Status = "retained"
			result.Message = "邮箱仍被其他平台占用，或尚未完成邮箱资产核对"
			summary.Retained++
			summary.Results = append(summary.Results, result)
			continue
		}
		for _, candidate := range candidates {
			if deleteErr := hme.DeleteMailbox(cleanupCtx, registerruntime.Mailbox{ID: candidate.SourceMailboxID, Email: candidate.Email, AccountID: candidate.SourceAccountID}); deleteErr != nil {
				if isHMEErrorCode(deleteErr, "mailbox_not_found") {
					_ = s.mailboxStore.MarkCleanup(cleanupCtx, candidate, true, "mailbox_not_found")
					result.Status = "not_found"
					summary.NotFound++
				} else {
					_ = s.mailboxStore.MarkCleanup(cleanupCtx, candidate, false, hmeCleanupMessage(deleteErr))
					result.Status = "failed"
					result.Message = hmeCleanupMessage(deleteErr)
					summary.Failed++
				}
			} else {
				_ = s.mailboxStore.MarkCleanup(cleanupCtx, candidate, true, "")
				result.Status = "deleted"
				summary.Deleted++
			}
			summary.Results = append(summary.Results, result)
		}
	}
	return summary
}

func (s *Server) deleteAccountsAndCleanup(ctx context.Context, tokens []string, accounts []map[string]any) (int, []map[string]any, hmeCleanupSummary, error) {
	removed, remaining, err := s.store.DeleteAccounts(tokens)
	if err != nil {
		return 0, nil, hmeCleanupSummary{}, err
	}
	return removed, remaining, s.cleanupHMEMailboxes(ctx, accounts), nil
}

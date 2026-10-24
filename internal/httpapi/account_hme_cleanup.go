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
		Email:     strings.TrimSpace(stringValue(mailbox["email"])),
		AccountID: strings.TrimSpace(stringValue(mailbox["account_id"])),
	}, true
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

func (s *Server) deleteAccountsAndCleanup(ctx context.Context, tokens []string, accounts []map[string]any) (int, []map[string]any, hmeCleanupSummary, error) {
	removed, remaining, err := s.store.DeleteAccounts(tokens)
	if err != nil {
		return 0, nil, hmeCleanupSummary{}, err
	}
	return removed, remaining, s.cleanupHMEMailboxes(ctx, accounts), nil
}

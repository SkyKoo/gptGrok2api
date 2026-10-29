package httpapi

import (
	"context"
	"errors"
	"strings"
	"time"

	mailboxstore "github.com/auucoder/gptgrok2api-go/internal/mailbox"
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
	Released int                `json:"released"`
	Skipped  int                `json:"skipped"`
	Retained int                `json:"retained"`
	NotFound int                `json:"not_found"`
	Failed   int                `json:"failed"`
	Results  []hmeCleanupResult `json:"results,omitempty"`
}

func hmeCleanupMessage(err error) string {
	var failure *registerruntime.Failure
	if errors.As(err, &failure) {
		return failure.Error()
	}
	return "上游邮箱删除失败"
}

func isHMEErrorCode(err error, code string) bool {
	var failure *registerruntime.Failure
	return errors.As(err, &failure) && failure.Code == code
}

func hmeMailboxFromAccount(account map[string]any) (registerruntime.Mailbox, bool) {
	mailbox := mapValue(account["registration_mailbox"])
	if provider := stringValue(mailbox["provider"]); !strings.EqualFold(provider, "icloud_hme") && !strings.EqualFold(provider, "hme") {
		return registerruntime.Mailbox{}, false
	}
	return registerruntime.Mailbox{
		ID:              strings.TrimSpace(stringValue(mailbox["id"])),
		Email:           strings.TrimSpace(stringValue(mailbox["email"])),
		AccountID:       strings.TrimSpace(stringValue(mailbox["account_id"])),
		SourceID:        strings.TrimSpace(stringValue(mailbox["source_id"])),
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

func (s *Server) cleanupHMEMailboxes(ctx context.Context, accounts []map[string]any, deletedBefore ...time.Time) hmeCleanupSummary {
	summary := hmeCleanupSummary{}
	summary.Results = make([]hmeCleanupResult, 0, len(accounts))
	if len(accounts) == 0 {
		return summary
	}
	if s.mailboxManager == nil || !s.mailboxManager.Enabled() {
		for _, account := range accounts {
			box, isHME := hmeMailboxFromAccount(account)
			result := hmeCleanupResult{AccountRef: accountPublicRef(account), Email: box.Email}
			if !isHME {
				result.Status = "not_hme_source"
				summary.Skipped++
			} else {
				result.Status = "retained"
				result.Message = "邮箱资源管理模块不可用，已保留上游邮箱"
				summary.Retained++
			}
			summary.Results = append(summary.Results, result)
		}
		return summary
	}
	summary.Enabled = true
	if s.mailboxStore != nil {
		return s.cleanupMailboxRegistry(ctx, accounts, summary, deletedBefore...)
	}
	return s.cleanupCoordinatedMailboxes(ctx, accounts, summary)
}

func (s *Server) cleanupCoordinatedMailboxes(ctx context.Context, accounts []map[string]any, summary hmeCleanupSummary) hmeCleanupSummary {
	cleanupCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	sessions := map[string]*mailboxstore.ResourceSession{}
	defer func() {
		for _, session := range sessions {
			session.Close()
		}
	}()
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
		sourceID := strings.TrimSpace(box.SourceID)
		session := sessions[sourceID]
		var err error
		if session == nil {
			session, err = s.mailboxManager.Open(cleanupCtx, sourceID, time.Second)
			if err == nil {
				sourceID = session.Source().ID
				sessions[sourceID] = session
			}
		}
		if err != nil {
			result.Status = "failed"
			result.Message = hmeCleanupMessage(err)
			summary.Failed++
			summary.Results = append(summary.Results, result)
			continue
		}
		if !boolValue(session.Source().Config["delete_hme_mailbox_on_account_delete"], false) {
			result.Status, result.Message = "retained", "邮箱来源未启用删除策略"
			summary.Retained++
			summary.Results = append(summary.Results, result)
			continue
		}
		platform := mailboxPlatform(account)
		accountID := firstNonEmpty(stringValue(account["id"]), stringValue(account["account_id"]), result.AccountRef)
		candidates, releaseErr := session.ReleaseForAccount(cleanupCtx, platform, accountID, box.Email)
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
			if deleteErr := session.Delete(cleanupCtx, candidate); deleteErr != nil {
				if errors.Is(deleteErr, mailboxstore.ErrProviderMailboxNotFound) {
					_ = session.MarkCleanup(cleanupCtx, candidate, true, "mailbox_not_found")
					result.Status = "not_found"
					summary.NotFound++
				} else {
					_ = session.MarkCleanup(cleanupCtx, candidate, false, hmeCleanupMessage(deleteErr))
					result.Status = "failed"
					result.Message = hmeCleanupMessage(deleteErr)
					summary.Failed++
				}
			} else {
				_ = session.MarkCleanup(cleanupCtx, candidate, true, "")
				result.Status = "deleted"
				summary.Deleted++
			}
			summary.Results = append(summary.Results, result)
		}
	}
	return summary
}

// Registry ownership is released before reading source settings or opening a
// provider session. Upstream cleanup is a separate, optional second step.
func (s *Server) cleanupMailboxRegistry(ctx context.Context, accounts []map[string]any, summary hmeCleanupSummary, deletedBefore ...time.Time) hmeCleanupSummary {
	for _, account := range accounts {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 35*time.Second)
		result := hmeCleanupResult{AccountRef: accountPublicRef(account), Email: firstNonEmpty(stringValue(mapValue(account["registration_mailbox"])["email"]), stringValue(account["email"]))}
		accountID := firstNonEmpty(stringValue(account["id"]), stringValue(account["account_id"]), result.AccountRef)
		assets, err := s.mailboxManager.ReleaseForAccount(cleanupCtx, mailboxPlatform(account), accountID, result.Email, deletedBefore...)
		if err != nil {
			result.Status, result.Message = "failed", "账号已删除，邮箱占用释放失败，请核对邮箱数据库"
			summary.Failed++
		} else if len(assets) == 0 {
			result.Status, result.Message = "retained", "没有匹配的平台占用，已保留上游邮箱"
			summary.Retained++
		} else {
			summary.Released++
			result.Status, result.Message = "released", "已释放平台占用，保留上游邮箱"
			for _, asset := range assets {
				// Disabled assets are retained even when the source opts into cleanup.
				if asset.Status != "available" {
					summary.Retained++
					continue
				}
				source, sourceErr := s.mailboxManager.SourceForMailbox(cleanupCtx, asset)
				if sourceErr != nil || !boolValue(source.Config["delete_hme_mailbox_on_account_delete"], false) {
					summary.Retained++
					if sourceErr != nil {
						result.Message = "已释放平台占用；邮箱来源无法匹配，已保留上游邮箱"
					}
					continue
				}
				_, deleteErr := s.mailboxManager.DeleteAsset(cleanupCtx, asset.ID, "account_cleanup")
				if errors.Is(deleteErr, mailboxstore.ErrMailboxRegistrationDisabled) {
					result.Message = "已释放平台占用；邮箱设置了渠道禁用，已保留上游邮箱"
					summary.Retained++
				} else if errors.Is(deleteErr, mailboxstore.ErrMailboxInUse) || errors.Is(deleteErr, mailboxstore.ErrMailboxBusy) {
					result.Message = "已释放平台占用；邮箱已被其他任务占用，已保留上游邮箱"
					summary.Retained++
				} else if deleteErr != nil {
					result.Status, result.Message = "failed", "已释放平台占用；上游邮箱删除失败，可在邮箱管理重试"
					summary.Failed++
				} else {
					result.Status, result.Message = "deleted", "已释放平台占用并删除上游邮箱"
					summary.Deleted++
				}
			}
		}
		cancel()
		summary.Results = append(summary.Results, result)
	}
	return summary
}

func (s *Server) deleteAccountsAndCleanup(ctx context.Context, tokens []string, accounts []map[string]any) (int, []map[string]any, hmeCleanupSummary, error) {
	deletedBefore := time.Now()
	removed, remaining, err := s.store.DeleteAccounts(tokens)
	if err != nil {
		return 0, nil, hmeCleanupSummary{}, err
	}
	return removed, remaining, s.cleanupHMEMailboxes(ctx, accounts, deletedBefore), nil
}

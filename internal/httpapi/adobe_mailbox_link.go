package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
	mailboxstore "github.com/auucoder/gptgrok2api-go/internal/mailbox"
)

var errAdobeMailboxMetadataMissing = errors.New("HME mailbox metadata is required for automatic reconciliation")
var errAdobeMailboxMetadataMismatch = errors.New("existing mailbox metadata does not match asset")
var errAdobeMailboxSourceUnsupported = errors.New("mailbox source does not support account association")

type adobeMailboxLinkResult struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

type adobeMailboxLinkSummary struct {
	Linked        int                      `json:"linked"`
	AlreadyLinked int                      `json:"already_linked"`
	Unmatched     int                      `json:"unmatched"`
	Conflicts     int                      `json:"conflicts"`
	Failed        int                      `json:"failed"`
	Results       []adobeMailboxLinkResult `json:"results"`
}

func (s *Server) adobeLinkMailboxes(w http.ResponseWriter, r *http.Request) {
	if !s.mailboxDBReady(w) {
		return
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if !decodeJSON(w, r, &body) {
		return
	}
	ids := uniqueStrings(body.IDs)
	if len(ids) == 0 || len(ids) > 100 {
		writeError(w, http.StatusBadRequest, "请选择 1–100 个 Adobe 账号", "invalid_request_error")
		return
	}
	summary := adobeMailboxLinkSummary{Results: make([]adobeMailboxLinkResult, 0, len(ids))}
	for _, id := range ids {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		changed, err := s.linkAdobeMailbox(ctx, id, false)
		cancel()
		result := adobeMailboxLinkResult{ID: id}
		switch {
		case err == nil && changed:
			summary.Linked++
			result.Status, result.Message = "linked", "已按邮箱关联现有资产"
		case err == nil:
			summary.AlreadyLinked++
			result.Status, result.Message = "already_linked", "已关联，无需重复操作"
		case errors.Is(err, mailboxstore.ErrMailboxNotFound):
			summary.Unmatched++
			result.Status, result.Message = "unmatched", "没有匹配的现有邮箱资产，请先在邮箱管理核对资产"
		case errors.Is(err, mailboxstore.ErrMailboxInUse):
			summary.Conflicts++
			result.Status, result.Message = "conflict", "邮箱已被其他 Adobe 账号或注册任务占用"
		case errors.Is(err, mailboxstore.ErrMailboxStateImmutable):
			summary.Conflicts++
			result.Status, result.Message = "conflict", "邮箱已删除或待清理，无法关联"
		case errors.Is(err, errAdobeMailboxMetadataMismatch):
			summary.Conflicts++
			result.Status, result.Message = "conflict", "账号已有的邮箱来源信息与资产不一致"
		case errors.Is(err, errAdobeMailboxSourceUnsupported):
			summary.Conflicts++
			result.Status, result.Message = "conflict", "该邮箱来源暂不支持账号关联"
		default:
			summary.Failed++
			result.Status, result.Message = "failed", "关联未完成；已有占用会保留，可重试此操作"
			if errors.Is(err, adobe.ErrNotFound) {
				result.Message = "Adobe 账号不存在，请刷新列表"
			}
		}
		summary.Results = append(summary.Results, result)
	}
	writeJSON(w, http.StatusOK, summary)
}

// Automatic reconciliation and manual linking share account/asset lock ordering.
// Re-read the account under its store lock so a stale list cannot resurrect an
// account deleted while reconciliation was collecting other providers.
func (s *Server) linkAdobeMailbox(ctx context.Context, id string, requireMetadata bool) (bool, error) {
	return s.adobeStore.LinkMailbox(id, func(a adobe.Account) (map[string]any, bool, error) {
		if requireMetadata {
			if _, _, ok := mailboxAccount(map[string]any{"registration_mailbox": a.RegistrationMailbox}); !ok {
				return nil, false, errAdobeMailboxMetadataMissing
			}
		}
		box, claimed, err := s.mailboxStore.LinkExistingAccount(ctx, "adobe", a.ID, a.RegistrationJobID, a.Email, func(box mailboxstore.Mailbox) error {
			return validateAdobeMailboxMetadata(a.RegistrationMailbox, box)
		})
		if err != nil {
			return nil, false, err
		}
		metadata := make(map[string]any, len(a.RegistrationMailbox)+8)
		for key, value := range a.RegistrationMailbox {
			metadata[key] = value
		}
		metadata["provider"], metadata["platform"] = "icloud_hme", "adobe"
		metadata["source_id"], metadata["source_type"] = box.SourceID, box.SourceType
		metadata["id"], metadata["source_mailbox_id"] = box.ID, box.SourceMailboxID
		metadata["account_id"], metadata["email"] = box.SourceAccountID, box.Email
		return metadata, claimed, nil
	})
}

func validateAdobeMailboxMetadata(metadata map[string]any, box mailboxstore.Mailbox) error {
	sourceType := strings.ToLower(strings.TrimSpace(box.SourceType))
	if sourceType != "hme" && sourceType != "icloud_hme" {
		return errAdobeMailboxSourceUnsupported
	}
	if provider := strings.ToLower(strings.TrimSpace(stringValue(metadata["provider"]))); provider != "" && provider != "hme" && provider != "icloud_hme" {
		return errAdobeMailboxMetadataMismatch
	}
	if email := strings.TrimSpace(stringValue(metadata["email"])); email != "" && !strings.EqualFold(email, box.Email) {
		return errAdobeMailboxMetadataMismatch
	}
	if platform := strings.TrimSpace(stringValue(metadata["platform"])); platform != "" && !strings.EqualFold(platform, "adobe") {
		return errAdobeMailboxMetadataMismatch
	}
	for key, expected := range map[string]string{"source_id": box.SourceID, "source_mailbox_id": box.SourceMailboxID, "account_id": box.SourceAccountID} {
		if actual := strings.TrimSpace(stringValue(metadata[key])); actual != "" && actual != expected {
			return errAdobeMailboxMetadataMismatch
		}
	}
	// Older registration results stored the upstream alias ID in id.
	if id := strings.TrimSpace(stringValue(metadata["id"])); id != "" && id != box.ID && id != box.SourceMailboxID {
		return errAdobeMailboxMetadataMismatch
	}
	if kind := strings.ToLower(strings.TrimSpace(stringValue(metadata["source_type"]))); kind != "" && kind != "hme" && kind != "icloud_hme" {
		return errAdobeMailboxMetadataMismatch
	}
	return nil
}

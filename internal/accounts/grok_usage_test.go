package accounts

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/store"
)

func TestGrokFeedbackRecordsChatMediaAndScopedLimits(t *testing.T) {
	root := t.TempDir()
	repository := store.New(filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"))
	if err := repository.SaveAccounts([]map[string]any{{"access_token": "usage-test", "enabled": true, "status": "active"}}); err != nil {
		t.Fatal(err)
	}
	defer repository.FlushAccounts()
	p := NewGrok(repository)
	account := Account{Token: "usage-test", Fields: map[string]any{}}
	p.Feedback(account, 200, nil)
	p.FeedbackIntent(&Lease{Account: account, intent: Intent{Kind: "image", Model: "grok-imagine-image-pro"}}, 429, errors.New("image_gen quota exceeded"))
	p.FeedbackIntent(&Lease{Account: account, intent: Intent{Kind: "video", Model: "grok-imagine-video"}}, 200, nil)
	rows, err := repository.AccountList()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(rows[0]["use_count"]) != "2" || fmt.Sprint(rows[0]["fail_count"]) != "1" {
		t.Fatalf("feedback lost or double counted: %#v", rows[0])
	}
	if rows[0]["status"] != "active" {
		t.Fatal("media quota changed account eligibility")
	}
}

package store

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func TestAccountUsageConcurrentPersistence(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "accounts.json")
	repository := New(path, filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"))
	if err := repository.SaveAccounts([]map[string]any{{"access_token": "usage-test", "status": "异常", "enabled": false}}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func(success bool) {
			defer wg.Done()
			if err := repository.RecordAccountUsage("usage-test", success); err != nil {
				t.Error(err)
			}
		}(i%3 != 0)
	}
	wg.Wait()
	if err := repository.FlushAccounts(); err != nil {
		t.Fatal(err)
	}
	reloaded := New(path, "", "")
	rows, err := reloaded.AccountList()
	if err != nil {
		t.Fatal(err)
	}
	item := rows[0]
	if fmt.Sprint(item["use_count"]) != "40" || fmt.Sprint(item["fail_count"]) != "20" {
		t.Fatalf("lost counts: %#v", item)
	}
	if item["last_used_at"] == nil || item["usage_started_at"] == nil {
		t.Fatal("missing observed timestamps")
	}
	if item["status"] != "异常" || item["enabled"] != false {
		t.Fatal("statistics changed eligibility")
	}
	if err := repository.RecordAccountUsage("deleted", true); err == nil {
		t.Fatal("must not recreate missing account")
	}
}

package register

import (
	"strings"
	"testing"
	"time"
)

func TestProfileForEmail(t *testing.T) {
	name, birthday := profileForEmail("netball.chute2x@icloud.com")
	if name != "Netball Chute X" {
		t.Fatalf("name=%q", name)
	}
	parsed, err := time.Parse("2006-01-02", birthday)
	if err != nil {
		t.Fatalf("birthday=%q: %v", birthday, err)
	}
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	oldest := today.AddDate(-50, 0, 0)
	youngest := today.AddDate(-20, 0, 0)
	if parsed.Before(oldest) || parsed.After(youngest) {
		t.Fatalf("birthday=%s outside 20-50 age window", birthday)
	}
}

func TestProfileForEmailFallbacks(t *testing.T) {
	name, birthday := profileForEmail("7@icloud.com")
	if name != "Openai User" || !strings.Contains(birthday, "-") {
		t.Fatalf("fallback profile name=%q birthday=%q", name, birthday)
	}
	name, _ = profileForEmail("solo@icloud.com")
	if name != "Solo User" {
		t.Fatalf("single-token profile name=%q", name)
	}
}

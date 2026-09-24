package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
)

func TestImagePollUsesTaskBudgetInsteadOfRequestTimeout(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"mapping":{}}`)), Request: r}, nil
	})}
	image := NewOpenAIImage("http://upstream.invalid", client, nil, 20*time.Millisecond)
	ctx, cancel := WithImageTaskTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	deadline, _ := ctx.Deadline()
	if !image.imagePollDeadline(ctx).Equal(deadline) {
		t.Fatal("request timeout shortened task budget")
	}
	start := time.Now()
	_, err := image.pollConversation(ctx, accounts.Account{}, "pending")
	if err == nil {
		t.Fatal("missing timeout")
	}
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond || elapsed > time.Second {
		t.Fatalf("incorrect polling budget: %s", elapsed)
	}
	shorter, stop := context.WithTimeout(ctx, time.Millisecond)
	defer stop()
	if d, _ := shorter.Deadline(); !image.imagePollDeadline(shorter).Equal(d) {
		t.Fatal("parent deadline ignored")
	}
}

func TestImageTaskDeadlineIsNotReset(t *testing.T) {
	original, cancel := WithImageTaskTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	nested, finish := WithImageTaskTimeout(original, 15*time.Minute)
	defer finish()
	a, _ := original.Deadline()
	b, _ := nested.Deadline()
	if !a.Equal(b) {
		t.Fatal("nested stage restarted the budget")
	}
	cancel()
	if !errors.Is(nested.Err(), context.Canceled) {
		t.Fatal("parent cancellation not propagated")
	}
}

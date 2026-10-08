//go:build cfm_live

package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/config"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
	"github.com/auucoder/gptgrok2api-go/internal/store"
)

// This opt-in probe sends only generated geometric fixtures. It never persists
// account feedback or exposes account identifiers/credentials in test output.
func TestLiveOpenAIMultimodalChat(t *testing.T) {
	if os.Getenv("CFM_LIVE_CHAT") != "1" {
		t.Skip("set CFM_LIVE_CHAT=1 explicitly; this test consumes ChatGPT quota")
	}
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal("cannot load local configuration")
	}
	repository := store.New(cfg.AccountsPath, cfg.AuthKeysPath, cfg.ConfigPath)
	pool := accounts.New(repository)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	skip, _ := strconv.Atoi(os.Getenv("CFM_LIVE_SKIP_ACCOUNTS"))
	if skip < 0 || skip > 2 {
		t.Fatal("CFM_LIVE_SKIP_ACCOUNTS must be between 0 and 2")
	}
	excluded := map[string]bool{}
	var lease *accounts.Lease
	for index := 0; index <= skip; index++ {
		lease, err = pool.ReserveMatching(ctx, []string{"basic", "super", "heavy"}, excluded, func(a accounts.Account) bool {
			return strings.Count(a.Token, ".") == 2 && (strings.EqualFold(stringValue(a.Fields["type"]), "free") || strings.EqualFold(stringValue(a.Fields["survival_plan_type"]), "free"))
		})
		if err != nil {
			t.Fatal("no eligible Free account available")
		}
		if index < skip {
			excluded[lease.Account.Token] = true
			pool.Release(lease)
		}
	}
	if os.Getenv("CFM_LIVE_NEWEST_TOKEN") == "1" {
		pool.Release(lease)
		items, _, readErr := repository.AccountSnapshot()
		if readErr != nil {
			t.Fatal("cannot inspect token expiration")
		}
		newest, chosen := time.Now().Unix(), ""
		for _, item := range items {
			token := firstStringValue(item, "access_token", "accessToken", "token")
			parts := strings.Split(token, ".")
			if len(parts) != 3 || excluded[token] || !(strings.EqualFold(stringValue(item["type"]), "free") || strings.EqualFold(stringValue(item["survival_plan_type"]), "free")) {
				continue
			}
			raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
			var claims struct {
				Exp int64 `json:"exp"`
			}
			if json.Unmarshal(raw, &claims) == nil && claims.Exp > newest {
				newest, chosen = claims.Exp, token
			}
		}
		lease, err = pool.ReserveMatching(ctx, []string{"basic", "super", "heavy"}, excluded, func(a accounts.Account) bool { return a.Token == chosen })
		if err != nil {
			t.Fatal("no eligible fresh Free token available")
		}
	}
	defer pool.Release(lease)
	proxy := proxyruntime.NewManager(cfg.ProxyURL, cfg.ProxyPool)
	client := &http.Client{Transport: proxyruntime.NewTransport(http.DefaultTransport), Timeout: 90 * time.Second}
	chat := NewOpenAIChat(NewOpenAIImage(cfg.OpenAIBaseURL, client, proxy, 90*time.Second))
	images := make([]OpenAIChatImage, 2)
	for i := range images {
		canvas := image.NewRGBA(image.Rect(0, 0, 320, 320))
		draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
		if i == 0 {
			for y := 60; y < 260; y++ {
				for x := 60; x < 260; x++ {
					if (x-160)*(x-160)+(y-160)*(y-160) < 10000 {
						canvas.Set(x, y, color.RGBA{220, 20, 30, 255})
					}
				}
			}
		} else {
			draw.Draw(canvas, image.Rect(60, 60, 260, 260), image.NewUniform(color.RGBA{20, 60, 220, 255}), image.Point{}, draw.Src)
		}
		var b bytes.Buffer
		if err := png.Encode(&b, canvas); err != nil {
			t.Fatal("fixture encoding failed")
		}
		images[i] = OpenAIChatImage{MessageIndex: 0, PartIndex: i + 1, Input: OpenAIImageInput{Name: "reference.png", MIME: "image/png", Data: b.Bytes()}}
	}
	request := protocol.ChatRequest{Model: "auto", Messages: []protocol.Message{{Role: "user", Content: []any{
		map[string]any{"type": "text", "text": "Do not generate images. Inspect the two attached images in order. Return only JSON with this shape: {\"references\":[{\"index\":1,\"color\":\"English color name\",\"shape\":\"English shape name\"},{\"index\":2,\"color\":\"English color name\",\"shape\":\"English shape name\"}],\"outputs\":[{\"reference_index\":1,\"prompt\":\"A new composition keeping the first reference subject on a beach\"},{\"reference_index\":2,\"prompt\":\"A new composition keeping the second reference subject in a forest\"}]}. Identify colors and shapes from the actual attachments, and write two complete creative prompts."},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "fixture:1"}},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "fixture:2"}},
	}}}}
	started := time.Now()
	text, _, err := chat.Complete(ctx, lease.Account, request, images...)
	if err != nil {
		var upstream *protocol.UpstreamError
		if errors.As(err, &upstream) {
			t.Fatalf("upstream failure: status=%d message=%s", upstream.Status, strings.ReplaceAll(upstream.Message, lease.Account.Token, "[redacted]"))
		}
		t.Fatalf("probe failed after %.1fs (error type %T)", time.Since(started).Seconds(), err)
	}
	t.Logf("elapsed_seconds=%.1f output=%s", time.Since(started).Seconds(), text)
	text = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(text), "```json"), "```"), "```"))
	var result struct {
		References []struct {
			Index        int
			Color, Shape string
		}
		Outputs []struct {
			ReferenceIndex int `json:"reference_index"`
			Prompt         string
		}
	}
	if json.Unmarshal([]byte(text), &result) != nil || len(result.References) != 2 || len(result.Outputs) != 2 {
		t.Fatal("response did not contain the requested two-reference/two-plan JSON")
	}
	if result.References[0].Index != 1 || !strings.EqualFold(result.References[0].Color, "red") || !strings.EqualFold(result.References[0].Shape, "circle") || result.References[1].Index != 2 || !strings.EqualFold(result.References[1].Color, "blue") || !strings.EqualFold(result.References[1].Shape, "square") {
		t.Fatal("image understanding did not match the fixtures")
	}
	for i, output := range result.Outputs {
		if output.ReferenceIndex != i+1 || strings.TrimSpace(output.Prompt) == "" {
			t.Fatal("plan did not preserve reference order")
		}
	}
}

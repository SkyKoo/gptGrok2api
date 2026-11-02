package provider

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/model"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

// FetchModes uses the same account and egress as chat. It never creates a
// conversation, follows redirects, or retries an upstream refusal.
func (g *GrokChat) FetchModes(ctx context.Context, url string, account accounts.Account) (model.GrokWebCatalog, error) {
	if g.Proxy != nil {
		ctx = proxyruntime.WithURL(ctx, g.Proxy.Resolve(account.Fields, false))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBufferString(`{"locale":"zh-CN"}`))
	if err != nil {
		return model.GrokWebCatalog{}, fmt.Errorf("invalid Grok Web modes endpoint")
	}
	for k, v := range headers(account) {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	client := *g.Client
	client.Jar = nil
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return model.GrokWebCatalog{}, fmt.Errorf("Grok Web model directory request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized {
			return model.GrokWebCatalog{}, fmt.Errorf("Grok Web model directory HTTP 401: %w", ErrGrokInvalidCredentials)
		}
		return model.GrokWebCatalog{}, fmt.Errorf("Grok Web model directory HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if err != nil || len(raw) > 1<<20 {
		return model.GrokWebCatalog{}, fmt.Errorf("invalid Grok Web model directory response")
	}
	return model.ParseGrokWebCatalog(raw)
}

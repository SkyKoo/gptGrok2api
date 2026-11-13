package provider

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/xaiquality"
)

// XAIQualityHeaders keeps direct and CPA-side probes on the same CLI protocol.
func XAIQualityHeaders() map[string]string {
	req := &http.Request{Header: make(http.Header)}
	setXAIHeaders(req, "$TOKEN$")
	headers := map[string]string{}
	for key := range req.Header {
		headers[key] = req.Header.Get(key)
	}
	return headers
}

// QualityResponse uses only the supplied fresh access token. It never renews a
// token or queries billing; CPA-managed credentials must use CPA's api-call.
func (p *XAIProbe) QualityResponse(ctx context.Context, access string, payload []byte) (xaiquality.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/responses", bytes.NewReader(payload))
	if err != nil {
		return xaiquality.Response{}, errors.New("invalid quality endpoint")
	}
	setXAIHeaders(req, access)
	client := *p.HTTP
	client.Timeout = 65 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.Jar = nil
	resp, err := client.Do(req)
	if err != nil {
		return xaiquality.Response{}, errors.New("quality request failed")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil || len(raw) > 2<<20 {
		return xaiquality.Response{}, errors.New("quality response unavailable")
	}
	return xaiquality.Response{Status: resp.StatusCode, Body: raw}, nil
}

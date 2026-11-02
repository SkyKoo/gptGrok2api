package provider

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
)

func imagineTestPNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewNRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func readImagineRequest(t *testing.T, reader *bufio.Reader) (string, map[string]any) {
	t.Helper()
	var request map[string]any
	for n := 0; n < 2; n++ {
		text, err := readClientText(reader)
		if err != nil {
			t.Errorf("read request: %v", err)
			return "", nil
		}
		if err := json.Unmarshal([]byte(text), &request); err != nil {
			t.Errorf("decode: %v", err)
			return "", nil
		}
	}
	input := request["item"].(map[string]any)["content"].([]any)[0].(map[string]any)
	return input["requestId"].(string), input["properties"].(map[string]any)
}

func TestImagineSocketCompletionProtocol(t *testing.T) {
	smallFinal := base64.StdEncoding.EncodeToString(imagineTestPNG(t))
	largePreview := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("preview"), 50000))
	cases := []struct {
		name      string
		count     int
		events    []map[string]any
		urls      []string
		wantError bool
	}{
		{name: "large_preview_is_not_final", count: 1, wantError: true, events: []map[string]any{
			{"type": "image", "order": 0, "url": "/preview.jpg", "blob": largePreview, "current_status": "generating", "percentage_complete": 40},
		}},
		{name: "progress_alone_does_not_complete", count: 1, wantError: true, events: []map[string]any{
			{"type": "image", "order": 0, "url": "/final.jpg", "blob": largePreview, "percentage_complete": 100},
		}},
		{name: "metadata_does_not_promote_cached_preview", count: 1, wantError: true, events: []map[string]any{
			{"type": "image", "order": 0, "url": "/preview.jpg", "blob": largePreview, "percentage_complete": 40},
			{"type": "json", "order": 0, "current_status": "completed"},
		}},
		{name: "small_terminal_blob", count: 1, events: []map[string]any{
			{"type": "image", "order": 0, "blob": largePreview, "percentage_complete": 40},
			{"type": "image", "order": 0, "blob": smallFinal, "current_status": "completed"},
		}},
		{name: "final_blob_before_terminal_metadata", count: 1, events: []map[string]any{
			{"type": "image", "order": 0, "blob": smallFinal, "percentage_complete": 100},
			{"type": "json", "order": 0, "current_status": "completed"},
		}},
		{name: "blob_after_terminal_metadata", count: 1, events: []map[string]any{
			{"type": "json", "order": 0, "current_status": "completed"},
			{"type": "image", "order": 0, "blob": smallFinal},
		}},
		{name: "final_asset_overrides_large_preview", count: 1, urls: []string{"https://assets.grok.com/original.jpg"}, events: []map[string]any{
			{"type": "image", "order": 0, "url": "/preview.jpg", "blob": largePreview, "percentage_complete": 40},
			{"type": "json", "order": 0, "url": "/original.jpg", "current_status": "completed"},
		}},
		{name: "final_url_before_terminal_metadata", count: 1, urls: []string{"https://assets.grok.com/original.jpg"}, events: []map[string]any{
			{"type": "image", "order": 0, "url": "/original.jpg", "blob": smallFinal, "percentage_complete": 100},
			{"type": "json", "order": 0, "current_status": "completed"},
		}},
		{name: "interleaved_results_keep_order", count: 2, urls: []string{"https://assets.grok.com/a.jpg", "https://assets.grok.com/b.jpg"}, events: []map[string]any{
			{"type": "image", "order": 0, "url": "/a-preview.jpg", "blob": largePreview, "percentage_complete": 10},
			{"type": "json", "order": 1, "url": "/b.jpg", "current_status": "completed"},
			{"type": "json", "order": 0, "url": "/a.jpg", "current_status": "completed"},
		}},
		{name: "partial_count_is_failure", count: 2, wantError: true, events: []map[string]any{
			{"type": "json", "order": 0, "url": "/a.jpg", "current_status": "completed"},
		}},
		{name: "foreign_and_unordered_events_are_ignored", count: 1, events: []map[string]any{
			{"type": "error", "request_id": "another-request", "err_code": "rate_limit_exceeded"},
			{"type": "json", "request_id": "another-request", "order": 0, "url": "/wrong.jpg", "current_status": "completed"},
			{"type": "json", "url": "/wrong.jpg", "current_status": "completed"},
			{"type": "image", "order": 0, "blob": smallFinal, "current_status": "completed"},
		}},
		{name: "invalid_final_bytes", count: 1, wantError: true, events: []map[string]any{
			{"type": "image", "order": 0, "blob": largePreview, "current_status": "completed"},
		}},
		{name: "upstream_resolution_downgrade", count: 1, wantError: true, events: []map[string]any{
			{"type": "json", "order": 0, "url": "/original.jpg", "resolution_name": "1k", "current_status": "completed"},
		}},
		{name: "untrusted_original_host", count: 1, wantError: true, events: []map[string]any{
			{"type": "json", "order": 0, "url": "https://untrusted.example/image.jpg", "current_status": "completed"},
		}},
		{name: "per_image_failure", count: 1, wantError: true, events: []map[string]any{
			{"type": "json", "order": 0, "current_status": "error", "err_message": "generation failed"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			properties := make(chan map[string]any, 1)
			server := newImagineTestServer(t, func(conn net.Conn, reader *bufio.Reader) {
				id, props := readImagineRequest(t, reader)
				properties <- props
				for _, event := range tc.events {
					if _, exists := event["request_id"]; !exists {
						event["request_id"] = id
					}
					writeServerText(t, conn, mustJSON(event))
				}
			})
			defer server.Close()
			socket := NewImagineSocket("ws"+strings.TrimPrefix(server.URL, "http"), nil, time.Second)
			results, err := socket.Generate(context.Background(), accounts.Account{Token: "test-sso"}, "a cat", "16:9", "2k", tc.count, true, true)
			if tc.wantError {
				if err == nil {
					t.Fatal("incomplete/invalid image accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(results) != tc.count {
				t.Fatalf("count=%d", len(results))
			}
			for i, result := range results {
				if !result.ImagineFinal {
					t.Fatal("final image not marked for validation")
				}
				if len(tc.urls) > 0 {
					if result.URL != tc.urls[i] || result.Base64 != "" {
						t.Fatalf("preview selected: %+v", result)
					}
				} else if result.Base64 != smallFinal || result.MIME != "image/png" {
					t.Fatal("small final bytes were replaced")
				}
			}
			props := <-properties
			if props["resolution_name"] != "2k" || props["num_generations"] != float64(tc.count) || props["aspect_ratio"] != "16:9" || props["enable_pro"] != true || props["skip_upsampler"] != true || props["enable_side_by_side"] != false {
				t.Fatalf("incorrect quality parameters: %+v", props)
			}
		})
	}
}

func TestImagineSocketCancellation(t *testing.T) {
	received := make(chan struct{})
	server := newImagineTestServer(t, func(conn net.Conn, reader *bufio.Reader) {
		readImagineRequest(t, reader)
		close(received)
		_, _ = reader.ReadByte() // request cancellation must close the connection
	})
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket := NewImagineSocket("ws"+strings.TrimPrefix(server.URL, "http"), nil, 5*time.Second)
	done := make(chan error, 1)
	go func() {
		_, err := socket.Generate(ctx, accounts.Account{}, "test", "1:1", "1k", 1, true, true)
		done <- err
	}()
	<-received
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not unblock image stream")
	}
}

func TestImagineSocketFragmentedTerminalFrame(t *testing.T) {
	raw := imagineTestPNG(t)
	server := newImagineTestServer(t, func(conn net.Conn, reader *bufio.Reader) {
		id, _ := readImagineRequest(t, reader)
		text := mustJSON(map[string]any{"type": "image", "order": 0, "request_id": id, "current_status": "completed", "blob": base64.StdEncoding.EncodeToString(raw)})
		// Fragment into small unmasked frames, with an interleaved ping.
		for offset := 0; offset < len(text); {
			end := offset + 50
			if end > len(text) {
				end = len(text)
			}
			opcode := byte(0)
			if offset == 0 {
				opcode = 1
			}
			if end == len(text) {
				opcode |= 0x80
			}
			if _, err := conn.Write(append([]byte{opcode, byte(end - offset)}, []byte(text[offset:end])...)); err != nil {
				return
			}
			if offset == 0 {
				_, _ = conn.Write([]byte{0x89, 0})
				if _, err := readClientText(reader); err != nil {
					return
				}
			}
			offset = end
		}
	})
	defer server.Close()
	socket := NewImagineSocket("ws"+strings.TrimPrefix(server.URL, "http"), nil, time.Second)
	results, err := socket.Generate(context.Background(), accounts.Account{}, "test", "1:1", "1k", 1, true, true)
	if err != nil || len(results) != 1 || results[0].Base64 != base64.StdEncoding.EncodeToString(raw) {
		t.Fatalf("fragment lost: %v", err)
	}
}

func TestImagineSocketRateLimitIsTypedAs429(t *testing.T) {
	server := newImagineTestServer(t, func(conn net.Conn, reader *bufio.Reader) {
		for i := 0; i < 2; i++ {
			if _, err := readClientText(reader); err != nil {
				t.Errorf("read client message: %v", err)
				return
			}
		}
		writeServerText(t, conn, `{"type":"error","err_code":"rate_limit_exceeded","err_msg":"Image rate limit exceeded"}`)
	})
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	socket := NewImagineSocket(wsURL, nil, 3*time.Second)
	_, err := socket.Generate(context.Background(), accounts.Account{Token: "test-sso"}, "a red cat", "1:1", "1k", 1, true, false)
	if err == nil {
		t.Fatal("expected rate-limit error")
	}
	var upstream *protocol.UpstreamError
	if !asUpstreamError(err, &upstream) || upstream.Status != http.StatusTooManyRequests {
		t.Fatalf("expected typed 429, got %T %v", err, err)
	}
	if !strings.Contains(upstream.Message, "Image rate limit exceeded") {
		t.Fatalf("unexpected error message: %q", upstream.Message)
	}
}

func TestDecodeImagineBlobAcceptsDataURL(t *testing.T) {
	raw := imagineTestPNG(t)
	encoded := "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)
	decoded, mime, ok := decodeImagineBlob(encoded)
	if !ok || mime != "image/png" || string(decoded) != string(raw) {
		t.Fatalf("unexpected data URL decode: ok=%v mime=%q bytes=%q", ok, mime, decoded)
	}
}

func newImagineTestServer(t *testing.T, handler func(net.Conn, *bufio.Reader)) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		hijacker, ok := writer.(http.Hijacker)
		if !ok {
			t.Errorf("test server does not support hijacking")
			return
		}
		conn, reader, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack websocket connection: %v", err)
			return
		}
		defer conn.Close()
		key := request.Header.Get("Sec-WebSocket-Key")
		hash := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "+base64.StdEncoding.EncodeToString(hash[:])+"\r\n\r\n")
		handler(conn, reader.Reader)
	}))
	server.Start()
	return server
}

func writeServerText(t *testing.T, conn net.Conn, value string) {
	t.Helper()
	payload := []byte(value)
	header := []byte{0x81}
	switch {
	case len(payload) < 126:
		header = append(header, byte(len(payload)))
	case len(payload) <= 65535:
		header = append(header, 126, byte(len(payload)>>8), byte(len(payload)))
	default:
		header = append(header, 127, 0, 0, 0, 0, byte(len(payload)>>24), byte(len(payload)>>16), byte(len(payload)>>8), byte(len(payload)))
	}
	if _, err := conn.Write(append(header, payload...)); err != nil {
		t.Errorf("write server frame: %v", err)
	}
}

func readClientText(reader *bufio.Reader) (string, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		return "", err
	}
	length := int64(header[1] & 0x7f)
	if length == 126 {
		var value [2]byte
		if _, err := io.ReadFull(reader, value[:]); err != nil {
			return "", err
		}
		length = int64(binary.BigEndian.Uint16(value[:]))
	} else if length == 127 {
		var value [8]byte
		if _, err := io.ReadFull(reader, value[:]); err != nil {
			return "", err
		}
		length = int64(binary.BigEndian.Uint64(value[:]))
	}
	if length > 1<<20 || header[1]&0x80 == 0 {
		return "", fmt.Errorf("invalid client frame")
	}
	var mask [4]byte
	if _, err := io.ReadFull(reader, mask[:]); err != nil {
		return "", err
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return "", err
	}
	for index := range payload {
		payload[index] ^= mask[index%4]
	}
	return string(payload), nil
}

func asUpstreamError(err error, target **protocol.UpstreamError) bool {
	value, ok := err.(*protocol.UpstreamError)
	if ok {
		*target = value
	}
	return ok
}

func TestImagineSocketModerationTerminatesWithoutWaitingForEOF(t *testing.T) {
	for _, response := range []string{
		`{"type":"json","current_status":"completed","moderated":true,"image_id":"PRIVATE"}`,
		`{"type":"error","err_code":3,"err_msg":"Content Moderated [WKE=grok:content-moderated] PRIVATE"}`,
	} {
		t.Run(response, func(t *testing.T) {
			release := make(chan struct{})
			server := newImagineTestServer(t, func(conn net.Conn, reader *bufio.Reader) {
				for n := 0; n < 2; n++ {
					if _, err := readClientText(reader); err != nil {
						return
					}
				}
				writeServerText(t, conn, response)
				<-release
			})
			defer server.Close()
			defer close(release)
			socket := NewImagineSocket("ws"+strings.TrimPrefix(server.URL, "http"), nil, time.Second)
			_, err := socket.Generate(context.Background(), accounts.Account{Token: "test-sso"}, "test prompt", "1:1", "1k", 1, true, true)
			var upstream *protocol.UpstreamError
			if !asUpstreamError(err, &upstream) || upstream.Status != http.StatusUnprocessableEntity {
				t.Fatalf("explicit moderation should return 422 before server closes, got %v", err)
			}
			if !strings.Contains(upstream.Message, "内容审核") || strings.Contains(upstream.Message, "PRIVATE") || upstream.Body != "" {
				t.Fatalf("unexpected moderation message: %v", err)
			}
		})
	}
}

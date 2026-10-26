package provider

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
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

func TestImagineSocketProtocolWaitsForFinalBlob(t *testing.T) {
	finalBytes := append([]byte{0xff, 0xd8, 0xff}, bytesOfSize(imagineFinalMinBytes+1024-3, 'f')...)
	previewBytes := bytesOfSize(imagineMediumMinBytes+1024, 'p')
	var received []map[string]any
	server := newImagineTestServer(t, func(conn net.Conn, reader *bufio.Reader) {
		for len(received) < 2 {
			message, err := readClientText(reader)
			if err != nil {
				t.Errorf("read client message: %v", err)
				return
			}
			var object map[string]any
			if err := json.Unmarshal([]byte(message), &object); err != nil {
				t.Errorf("decode client message: %v", err)
				return
			}
			received = append(received, object)
		}
		writeServerText(t, conn, `{"type":"json","current_status":"generating"}`)
		writeServerText(t, conn, fmt.Sprintf(`{"type":"image","url":"/images/test-id.png","blob":%q}`, base64.StdEncoding.EncodeToString(previewBytes)))
		writeServerText(t, conn, fmt.Sprintf(`{"type":"image","url":"/images/test-id.jpg","blob":%q}`, base64.StdEncoding.EncodeToString(finalBytes)))
	})
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	socket := NewImagineSocket(wsURL, nil, 3*time.Second)
	results, err := socket.Generate(context.Background(), accounts.Account{Token: "test-sso"}, "a red cat", "16:9", 1, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].MIME != "image/jpeg" {
		t.Fatalf("unexpected final result: %#v", results)
	}
	decoded, err := base64.StdEncoding.DecodeString(results[0].Base64)
	if err != nil || string(decoded) != string(finalBytes) {
		t.Fatalf("preview blob was returned instead of final blob: err=%v size=%d", err, len(decoded))
	}
	if len(received) != 2 || received[0]["type"] != "conversation.item.create" || received[1]["type"] != "conversation.item.create" {
		t.Fatalf("unexpected request sequence: %#v", received)
	}
	requestItem := received[1]["item"].(map[string]any)
	content := requestItem["content"].([]any)
	input := content[0].(map[string]any)
	if input["text"] != "a red cat" || input["requestId"] == "" {
		t.Fatalf("unexpected imagine request: %#v", input)
	}
	properties := input["properties"].(map[string]any)
	if properties["aspect_ratio"] != "16:9" || properties["enable_nsfw"] != true || properties["enable_pro"] != false {
		t.Fatalf("unexpected imagine properties: %#v", properties)
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
	_, err := socket.Generate(context.Background(), accounts.Account{Token: "test-sso"}, "a red cat", "1:1", 1, true, false)
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
	raw := []byte("image-bytes")
	encoded := "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)
	decoded, mime, ok := decodeImagineBlob(encoded, "")
	if !ok || mime != "image/png" || string(decoded) != string(raw) {
		t.Fatalf("unexpected data URL decode: ok=%v mime=%q bytes=%q", ok, mime, decoded)
	}
}

func bytesOfSize(size int, value byte) []byte {
	result := make([]byte, size)
	for index := range result {
		result[index] = value
	}
	return result
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

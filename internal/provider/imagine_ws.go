package provider

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

// ImagineSocket is a deliberately small RFC 6455 client. The image endpoint
// only needs text frames, so keeping this transport local avoids another
// runtime dependency for the Go service.
type ImagineSocket struct {
	URL     string
	Client  *http.Client
	Timeout time.Duration
	Proxy   *proxyruntime.Manager
}

func NewImagineSocket(urlValue string, client *http.Client, timeout time.Duration) *ImagineSocket {
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	return &ImagineSocket{URL: urlValue, Client: client, Timeout: timeout}
}

// Generate waits for terminal metadata. Blob size and file extension do not
// distinguish a preview from a completed image in the Imagine protocol.
func (i *ImagineSocket) Generate(ctx context.Context, account accounts.Account, prompt, aspectRatio, resolution string, count int, enableNSFW, enablePro bool) ([]ImageResult, error) {
	if count < 1 || count > 10 || (resolution != "1k" && resolution != "2k") {
		return nil, fmt.Errorf("invalid Imagine count or resolution")
	}
	timeout := i.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := i.dial(ctx, account)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if err := conn.writeText(mustJSON(protocol.BuildResetMessage())); err != nil {
		return nil, err
	}
	requestID := randomUUID()
	if err := conn.writeText(mustJSON(protocol.BuildImagineRequest(requestID, prompt, aspectRatio, resolution, count, enableNSFW, enablePro))); err != nil {
		return nil, err
	}
	states := make([]imagineImageState, count)
	deadline, _ := ctx.Deadline()
	for {
		frame, err := conn.readText(ctx, deadline)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("Grok 图片流在取得全部最终原图前中断: %w", err)
		}
		var event imagineImageEvent
		if json.Unmarshal([]byte(frame), &event) != nil {
			continue
		}
		// One socket owns one request. Out-of-band responses cannot complete it.
		if event.RequestID != "" && event.RequestID != requestID {
			continue
		}
		if event.Type != "image" && event.Type != "json" && event.Type != "error" {
			continue
		}
		if event.Moderated {
			return nil, imagineModerationError()
		}
		if event.Type == "error" || event.Status == "error" {
			message := imagineFirstString(event.ErrorMessage, imagineFirstString(event.ErrorMessageAlt, imagineFirstString(event.Error, "imagine websocket error")))
			if isMediaModerationError(message) || isMediaModerationError(fmt.Sprint(event.ErrorCode)) {
				return nil, imagineModerationError()
			}
			status := http.StatusBadGateway
			if isImagineRateLimit(event.ErrorCode, message) {
				status = http.StatusTooManyRequests
			} else if resolution == "2k" && (strings.EqualFold(strings.TrimSpace(message), "Invalid argument") || strings.EqualFold(fmt.Sprint(event.ErrorCode), "invalid_argument")) {
				return nil, &protocol.UpstreamError{Status: http.StatusBadRequest, Message: "Grok 拒绝了当前 2K 请求参数，请改用 1K；尚未确认此账号支持 2K，未自动重试或降级", Endpoint: "/ws/imagine/listen"}
			}
			return nil, &protocol.UpstreamError{Status: status, Message: "imagine websocket error: " + message, Endpoint: "/ws/imagine/listen"}
		}
		// The official client correlates all preview/metadata events by order.
		// An absent order is not implicitly image zero, especially for n > 1.
		if event.Order == nil || *event.Order < 0 || *event.Order >= count {
			continue
		}
		state := &states[*event.Order]
		if state.ready {
			continue
		}
		if event.URL != "" && (event.Status == "completed" || state.completed || (event.Type == "image" && event.Percentage != nil && *event.Percentage >= 100)) {
			state.url = event.URL
		}
		if event.Blob != "" && (event.Status == "completed" || state.completed || (event.Type == "image" && event.Percentage != nil && *event.Percentage >= 100)) {
			state.finalBlob = event.Blob
		}
		if event.Resolution != "" {
			state.resolution = strings.ToLower(event.Resolution)
		}
		if event.Status == "completed" {
			state.completed = true
		}
		if !state.completed {
			continue
		}
		if resolution == "2k" && state.resolution != "" && state.resolution != "2k" {
			return nil, &protocol.UpstreamError{Status: http.StatusBadGateway, Message: "Grok 未按请求返回 2K 原图，请检查账号的 2K 权限；未自动重试或降级"}
		}
		if state.url != "" {
			original, err := imagineOriginalURL(state.url)
			if err != nil {
				return nil, err
			}
			// The completed asset is authoritative even if a stream blob exists.
			state.result = ImageResult{URL: original, ImagineFinal: true}
			state.ready = true
		} else if state.finalBlob != "" {
			// Only use explicitly finished bytes (100% or at/after completion).
			// Never promote a previously cached preview when metadata completes.
			raw, mime, ok := decodeImagineBlob(state.finalBlob)
			if !ok {
				return nil, fmt.Errorf("Grok 最终图片内容无效或不完整")
			}
			state.result = ImageResult{Base64: base64.StdEncoding.EncodeToString(raw), MIME: mime, ImagineFinal: true}
			state.ready = true
		}
		results := make([]ImageResult, 0, count)
		for _, state := range states {
			if state.ready {
				results = append(results, state.result)
			}
		}
		if len(results) == count {
			return results, nil
		}
	}
}

type imagineImageEvent struct {
	Type            string   `json:"type"`
	RequestID       string   `json:"request_id"`
	Order           *int     `json:"order"`
	Status          string   `json:"current_status"`
	URL             string   `json:"url"`
	Blob            string   `json:"blob"`
	Resolution      string   `json:"resolution_name"`
	Percentage      *float64 `json:"percentage_complete"`
	Moderated       bool     `json:"moderated"`
	ErrorCode       any      `json:"err_code"`
	ErrorMessage    string   `json:"err_msg"`
	ErrorMessageAlt string   `json:"err_message"`
	Error           any      `json:"error"`
}

type imagineImageState struct {
	completed, ready           bool
	url, resolution, finalBlob string
	result                     ImageResult
}

func imagineOriginalURL(value string) (string, error) {
	parsed, err := url.Parse(absoluteAssetURL(value))
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Port() != "" || (parsed.Hostname() != "assets.grok.com" && parsed.Hostname() != "grok.com" && parsed.Hostname() != "imagine-public.x.ai") {
		return "", fmt.Errorf("Grok 返回了无效的最终原图地址")
	}
	return parsed.String(), nil
}

func decodeImagineBlob(blob string) ([]byte, string, bool) {
	encoded := strings.TrimSpace(blob)
	if header, value, ok := strings.Cut(encoded, ","); ok && strings.Contains(strings.ToLower(header), "base64") {
		encoded = strings.TrimSpace(value)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, "", false
	}
	mime, err := validatedImagineImageMIME(raw)
	return raw, mime, err == nil
}

func validatedImagineImageMIME(raw []byte) (string, error) {
	mime, err := validatedImageMIME(raw)
	if err != nil {
		return "", &protocol.UpstreamError{Status: http.StatusBadGateway, Message: "Grok 最终原图无效、损坏或超出图片大小限制"}
	}
	return mime, nil
}

func isImagineRateLimit(code any, message string) bool {
	value := strings.ToLower(strings.TrimSpace(fmt.Sprint(code)))
	if value == "rate_limit_exceeded" || value == "rate_limited" || value == "429" || value == "usage_pool_exhausted" || value == "usage_limit_reached" || value == "concurrency_limit" {
		return true
	}
	lower := strings.ToLower(message)
	return strings.Contains(lower, "rate limit") || strings.Contains(lower, "too many") || strings.Contains(lower, "quota exceeded")
}

func imagineFirstString(value, fallback any) string {
	text := strings.TrimSpace(fmt.Sprint(value))
	if text == "" || text == "<nil>" {
		return strings.TrimSpace(fmt.Sprint(fallback))
	}
	return text
}

func (i *ImagineSocket) dial(ctx context.Context, account accounts.Account) (*wsConn, error) {
	parsed, err := url.Parse(i.URL)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return nil, fmt.Errorf("invalid imagine websocket URL")
	}
	address := parsed.Host
	if !strings.Contains(address, ":") {
		if parsed.Scheme == "wss" {
			address += ":443"
		} else {
			address += ":80"
		}
	}
	ctx = i.Proxy.RequestContext(ctx, account.Fields, false)
	proxyURL := proxyruntime.URLFromContext(ctx)
	network, err := proxyruntime.DialContext(ctx, address, proxyURL)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme == "wss" {
		tlsNetwork := tls.Client(network, &tls.Config{ServerName: parsed.Hostname(), MinVersion: tls.VersionTLS12})
		if err := tlsNetwork.HandshakeContext(ctx); err != nil {
			_ = network.Close()
			return nil, err
		}
		network = tlsNetwork
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = network.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { _ = network.Close() })
	defer stop()
	conn := &wsConn{conn: network, read: bufio.NewReader(network)}
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		network.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	cookie := firstString(account.Fields["cookie_header"], "")
	if cookie == "" {
		token := strings.TrimPrefix(account.Token, "sso=")
		cookie = "sso=" + token + "; sso-rw=" + token
	}
	request := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\nOrigin: https://grok.com\r\nReferer: https://grok.com/imagine\r\nCookie: %s\r\nUser-Agent: Mozilla/5.0\r\n\r\n", parsed.RequestURI(), parsed.Host, key, cookie)
	if _, err := io.WriteString(network, request); err != nil {
		network.Close()
		return nil, err
	}
	response, err := http.ReadResponse(conn.read, &http.Request{Method: http.MethodGet, URL: parsed})
	if err != nil {
		network.Close()
		return nil, err
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		network.Close()
		return nil, fmt.Errorf("imagine websocket handshake returned HTTP %d", response.StatusCode)
	}
	accept := response.Header.Get("Sec-WebSocket-Accept")
	hash := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	if accept != base64.StdEncoding.EncodeToString(hash[:]) {
		network.Close()
		return nil, fmt.Errorf("invalid websocket handshake")
	}
	return conn, nil
}

type wsConn struct {
	conn net.Conn
	read *bufio.Reader
}

func (w *wsConn) Close() error { return w.conn.Close() }

func (w *wsConn) writeText(value string) error {
	return w.writeFrame(0x1, []byte(value))
}

func (w *wsConn) writeFrame(opcode byte, payload []byte) error {
	key := make([]byte, 4)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	header := []byte{0x80 | opcode}
	length := len(payload)
	switch {
	case length < 126:
		header = append(header, 0x80|byte(length))
	case length <= 65535:
		header = append(header, 0x80|126, byte(length>>8), byte(length))
	default:
		header = append(header, 0x80|127)
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(length))
		header = append(header, buf[:]...)
	}
	header = append(header, key...)
	for index := range payload {
		payload[index] ^= key[index%4]
	}
	if _, err := w.conn.Write(header); err != nil {
		return err
	}
	_, err := w.conn.Write(payload)
	return err
}

func (w *wsConn) readText(ctx context.Context, deadline time.Time) (string, error) {
	if err := w.conn.SetReadDeadline(deadline); err != nil {
		return "", err
	}
	var message []byte
	fragmented := false
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		header := make([]byte, 2)
		if _, err := io.ReadFull(w.read, header); err != nil {
			return "", err
		}
		fin := header[0]&0x80 != 0
		opcode := header[0] & 0x0f
		masked := header[1]&0x80 != 0
		length := int64(header[1] & 0x7f)
		if length == 126 {
			var buf [2]byte
			if _, err := io.ReadFull(w.read, buf[:]); err != nil {
				return "", err
			}
			length = int64(binary.BigEndian.Uint16(buf[:]))
		} else if length == 127 {
			var buf [8]byte
			if _, err := io.ReadFull(w.read, buf[:]); err != nil {
				return "", err
			}
			value := binary.BigEndian.Uint64(buf[:])
			if value > 32<<20 {
				return "", fmt.Errorf("websocket frame too large")
			}
			length = int64(value)
		}
		if length > 32<<20 {
			return "", fmt.Errorf("websocket frame too large")
		}
		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(w.read, mask[:]); err != nil {
				return "", err
			}
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(w.read, payload); err != nil {
			return "", err
		}
		if masked {
			for index := range payload {
				payload[index] ^= mask[index%4]
			}
		}
		if opcode == 0x8 {
			return "", io.EOF
		}
		if opcode == 0x9 {
			_ = w.writeFrame(0xA, payload)
			continue
		}
		if opcode != 0x1 && opcode != 0x0 {
			continue
		}
		if opcode == 0x1 {
			if fragmented {
				return "", fmt.Errorf("unexpected websocket text frame")
			}
			message = payload
		} else {
			if !fragmented {
				return "", fmt.Errorf("unexpected websocket continuation")
			}
			if len(message)+len(payload) > 32<<20 {
				return "", fmt.Errorf("websocket message too large")
			}
			message = append(message, payload...)
		}
		fragmented = !fin
		if fin {
			return string(message), nil
		}
	}
}

func mustJSON(value map[string]any) string { raw, _ := json.Marshal(value); return string(raw) }

func randomUUID() string {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(raw)
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}

func boolValue(value any) bool { typed, _ := value.(bool); return typed }

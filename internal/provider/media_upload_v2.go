package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
)

// Imagine's current web client uses init -> signed PUT -> complete -> status.
// Every mutation is sent once. A refused or uncertain upload never falls back
// to the legacy endpoint or to text-only generation.
func (m *Media) uploadV2(ctx context.Context, account accounts.Account, filename, mime string, raw []byte) (map[string]any, error) {
	timeout := m.RequestTimeout
	if timeout <= 0 || timeout > 90*time.Second {
		timeout = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Resolve once so all steps retain the account's daily route, including direct.
	ctx = m.Proxy.RequestContext(ctx, account.Fields, false)
	base := m.AssetUploadURL
	init, err := m.uploadJSON(ctx, account, "init", http.MethodPost, base+"/init", map[string]any{
		"fileName": filename, "fileMimeType": mime, "fileSource": "IMAGINE_SELF_UPLOAD_FILE_SOURCE",
		"sizeBytes": strconv.Itoa(len(raw)), "multipartSupported": false,
	})
	if err != nil {
		return nil, err
	}
	id, _ := init["uploadId"].(string)
	method, _ := init["uploadMethod"].(string)
	put, _ := init["singlePut"].(map[string]any)
	if id == "" || len(id) > 512 || (method != "" && method != "UPLOAD_METHOD_UNSPECIFIED" && method != "UPLOAD_METHOD_SINGLE_PUT") {
		return nil, uploadFailure("init", "invalid_upload_response", 502)
	}
	if err = m.uploadSignedFile(ctx, put, raw); err != nil {
		return nil, err
	}
	result, err := m.uploadJSON(ctx, account, "complete", http.MethodPost, base+"/complete", map[string]any{
		"presigned": map[string]any{"uploadId": id, "completedParts": []any{}},
	})
	if err != nil {
		return nil, err
	}
	stage := "complete"
	delay := 150 * time.Millisecond
	for {
		if returned, _ := result["uploadId"].(string); returned != "" && returned != id {
			return nil, uploadFailure(stage, "invalid_upload_response", 502)
		}
		status, _ := result["status"].(string)
		if failure := result["terminalError"]; failure != nil && failure != "" {
			return nil, uploadFailure(stage, "upload_processing_failed", 422)
		}
		switch status {
		case "ERROR", "ABORTED", "EXPIRED":
			return nil, uploadFailure(stage, "upload_processing_failed", 422)
		}
		if metadata, _ := result["fileMetadata"].(map[string]any); validUploadMetadata(metadata) {
			return metadata, nil
		}
		if status == "SUCCESS" {
			// The official status response may only contain assetId. Resolve metadata
			// on the same API origin; never use a response-provided arbitrary URL.
			asset, _ := result["assetId"].(string)
			if !validUploadAssetID(asset) {
				return nil, uploadFailure("status", "invalid_upload_response", 502)
			}
			metadata, err := m.uploadJSON(ctx, account, "metadata", http.MethodPost, strings.TrimSuffix(base, "/upload-file-v2")+"/file-metadata/"+url.PathEscape(asset), nil)
			if err != nil {
				return nil, err
			}
			if !validUploadMetadata(metadata) {
				return nil, uploadFailure("metadata", "invalid_upload_response", 502)
			}
			return metadata, nil
		}
		if stage == "status" {
			switch status {
			case "INITIALIZED", "UPLOADING", "UPLOADED", "PROCESSING", "RETRYING":
			default:
				return nil, uploadFailure(stage, "invalid_upload_response", 502)
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, uploadErrorAt(ctx.Err(), "status")
		case <-timer.C:
		}
		result, err = m.uploadJSON(ctx, account, "status", http.MethodGet, base+"/status?uploadId="+url.QueryEscape(id), nil)
		if err != nil {
			return nil, err
		}
		stage = "status"
		delay = delay * 3 / 2
		if delay > 3*time.Second {
			delay = 3 * time.Second
		}
	}
}

func validUploadMetadata(v map[string]any) bool {
	id, _ := v["fileMetadataId"].(string)
	uri, _ := v["fileUri"].(string)
	return strings.TrimSpace(id) != "" && strings.TrimSpace(uri) != ""
}
func validUploadAssetID(id string) bool {
	if id == "" || len(id) > 256 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}
func uploadFailure(stage, kind string, status int) *MediaUploadError {
	return &MediaUploadError{Stage: stage, Kind: kind, Status: status, ContentType: "other"}
}
func uploadErrorAt(err error, stage string) error {
	e := safeMediaUploadError(err).(*MediaUploadError)
	e.Stage = stage
	if stage == "storage_put" && (e.Kind == "authentication_required" || e.Kind == "upload_forbidden") {
		e.Kind = "storage_upload_rejected"
	}
	return e
}

func (m *Media) uploadClient() *http.Client {
	client := *m.Client
	client.Jar = nil
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}
func (m *Media) uploadJSON(ctx context.Context, account accounts.Account, stage, method, endpoint string, payload any) (map[string]any, error) {
	var raw []byte
	if payload != nil {
		var err error
		raw, err = json.Marshal(payload)
		if err != nil {
			return nil, uploadErrorAt(err, stage)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, uploadErrorAt(err, stage)
	}
	applyMediaHeaders(req, account, "https://grok.com", "https://grok.com/")
	req.Header.Set("Content-Type", "application/json")
	response, err := m.uploadClient().Do(req)
	if err != nil {
		return nil, uploadErrorAt(err, stage)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 || strings.EqualFold(response.Header.Get("Cf-Mitigated"), "challenge") || strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/html") {
		err := readMediaUploadError(response).(*MediaUploadError)
		if response.StatusCode >= 200 && response.StatusCode < 300 && !err.Challenge {
			err.Status = 502
			err.Kind = "invalid_upload_response"
		}
		return nil, uploadErrorAt(err, stage)
	}
	raw, err = io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil {
		return nil, uploadErrorAt(err, stage)
	}
	var body map[string]any
	if len(raw) > 1<<20 || json.Unmarshal(raw, &body) != nil || body == nil {
		return nil, uploadFailure(stage, "invalid_upload_response", 502)
	}
	return body, nil
}

func (m *Media) uploadSignedFile(ctx context.Context, put map[string]any, raw []byte) error {
	target, _ := put["url"].(string)
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Fragment != "" || !(u.Hostname() == "storage.googleapis.com" || strings.HasSuffix(u.Hostname(), ".storage.googleapis.com")) {
		return uploadFailure("storage_put", "invalid_upload_target", 502)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, bytes.NewReader(raw))
	if err != nil {
		return uploadErrorAt(err, "storage_put")
	}
	headers, _ := put["requiredHeaders"].(map[string]any)
	if put["requiredHeaders"] != nil && headers == nil {
		return uploadFailure("storage_put", "invalid_upload_response", 502)
	}
	for name, value := range headers {
		key := strings.ToLower(name)
		text, ok := value.(string)
		allowed := key == "content-type" || key == "content-md5" || key == "content-disposition" || key == "content-length" || key == "cache-control" || key == "origin" || strings.HasPrefix(key, "x-goog-") || strings.HasPrefix(key, "x-amz-")
		if !ok || !allowed || strings.ContainsAny(text, "\r\n") || key == "content-length" && text != strconv.Itoa(len(raw)) {
			return uploadFailure("storage_put", "invalid_upload_response", 502)
		}
		req.Header.Set(name, text)
	}
	// No account cookies, authorization, browser headers or cookie jar go to storage.
	response, err := m.uploadClient().Do(req)
	if err != nil {
		return uploadErrorAt(err, "storage_put")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 || strings.EqualFold(response.Header.Get("Cf-Mitigated"), "challenge") {
		return uploadErrorAt(readMediaUploadError(response), "storage_put")
	}
	_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if err != nil {
		return uploadErrorAt(err, "storage_put")
	}
	return nil
}

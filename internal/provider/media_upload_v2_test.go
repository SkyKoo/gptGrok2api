package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

const v2TestInit = `{"uploadId":"upload-1","assetId":"asset-1","uploadMethod":"UPLOAD_METHOD_SINGLE_PUT","singlePut":{"url":"https://storage.googleapis.com/cfm-test/file?signature=SECRET","requiredHeaders":{"Content-Type":"image/jpeg","Content-Length":"4","Content-Disposition":"inline"}}}`
const v2TestMetadata = `{"fileMetadataId":"asset-1","fileUri":"users/test/source.jpeg"}`

func v2Response(r *http.Request, status int, body string) *http.Response {
	return &http.Response{Request: r, StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestMediaUploadV2PreservesFileAccountRouteAndStorageIsolation(t *testing.T) {
	for _, mode := range []string{"inline", "poll", "metadata"} {
		t.Run(mode, func(t *testing.T) {
			for _, route := range []string{"direct", "http://proxy.example:8080"} {
				t.Run(route, func(t *testing.T) {
					var paths []string
					jar, _ := cookiejar.New(nil)
					u, _ := url.Parse("https://storage.googleapis.com/")
					jar.SetCookies(u, []*http.Cookie{{Name: "unrelated-secret", Value: "SECRET"}})
					client := &http.Client{Jar: jar, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
						paths = append(paths, r.URL.Path)
						expected := route
						if route == "direct" {
							expected = ""
						}
						actual, selected := proxyruntime.URLSelectionFromContext(r.Context())
						if !selected || actual != expected {
							t.Fatalf("wrong route: selected=%t value=%s", selected, actual)
						}
						if r.URL.Host == "storage.googleapis.com" {
							if r.Method != "PUT" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("Referer") != "" {
								t.Fatal("storage received account credentials or wrong method")
							}
							raw, _ := io.ReadAll(r.Body)
							if !bytes.Equal(raw, []byte{0, 1, 2, 3}) || r.ContentLength != 4 || r.Header.Get("Content-Disposition") != "inline" {
								t.Fatal("uploaded bytes/required headers changed")
							}
							return v2Response(r, 200, ""), nil
						}
						if r.Header.Get("Cookie") != "sso=account-secret; sso-rw=account-secret" {
							t.Fatal("account changed during upload")
						}
						switch r.URL.Path {
						case "/rest/app-chat/upload-file-v2/init":
							var body map[string]any
							_ = json.NewDecoder(r.Body).Decode(&body)
							if body["fileName"] != "original.jpeg" || body["fileMimeType"] != "image/jpeg" || body["fileSource"] != "IMAGINE_SELF_UPLOAD_FILE_SOURCE" || body["sizeBytes"] != "4" || body["multipartSupported"] != false {
								t.Fatalf("bad init: %v", body)
							}
							return v2Response(r, 200, v2TestInit), nil
						case "/rest/app-chat/upload-file-v2/complete":
							var body struct {
								Presigned struct {
									UploadID string `json:"uploadId"`
									Parts    []any  `json:"completedParts"`
								} `json:"presigned"`
							}
							_ = json.NewDecoder(r.Body).Decode(&body)
							if body.Presigned.UploadID != "upload-1" || body.Presigned.Parts == nil || len(body.Presigned.Parts) != 0 {
								t.Fatal("complete lost upload identity")
							}
							if mode == "inline" {
								return v2Response(r, 200, `{"fileMetadata":`+v2TestMetadata+`}`), nil
							}
							return v2Response(r, 200, `{"uploadId":"upload-1"}`), nil
						case "/rest/app-chat/upload-file-v2/status":
							if r.Method != "GET" || r.URL.Query().Get("uploadId") != "upload-1" {
								t.Fatal("wrong status identity")
							}
							if mode == "metadata" {
								return v2Response(r, 200, `{"uploadId":"upload-1","status":"SUCCESS","assetId":"asset-1"}`), nil
							}
							return v2Response(r, 200, `{"uploadId":"upload-1","status":"SUCCESS","fileMetadata":`+v2TestMetadata+`}`), nil
						case "/rest/app-chat/file-metadata/asset-1":
							return v2Response(r, 200, v2TestMetadata), nil
						default:
							t.Fatalf("unexpected request %s", r.URL.Path)
							return nil, nil
						}
					})}
					media := NewMedia(client, "", "", "https://grok.com/rest/app-chat/upload-file-v2", "", 3*time.Second)
					media.SetProxyManager(proxyruntime.NewManager("http://ignored-global.example:8080", nil))
					id, uri, err := media.Upload(context.Background(), accounts.Account{Token: "account-secret", Fields: map[string]any{"proxy": route}}, "original.jpeg", "image/jpeg", base64.StdEncoding.EncodeToString([]byte{0, 1, 2, 3}))
					if err != nil || id != "asset-1" || uri != "users/test/source.jpeg" {
						t.Fatalf("upload failed: %v", err)
					}
					expected := []string{"/rest/app-chat/upload-file-v2/init", "/cfm-test/file", "/rest/app-chat/upload-file-v2/complete"}
					if mode != "inline" {
						expected = append(expected, "/rest/app-chat/upload-file-v2/status")
					}
					if mode == "metadata" {
						expected = append(expected, "/rest/app-chat/file-metadata/asset-1")
					}
					if !reflect.DeepEqual(paths, expected) {
						t.Fatalf("unexpected extra calls: %v", paths)
					}
				})
			}
		})
	}
}

func TestMediaUploadV2FailureNeverRetriesOrFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name, stage, kind string
		status            int
		challenge         bool
		wantStatus        int
	}{
		{"init challenge", "init", "verification_required", 403, true, 403},
		{"complete challenge", "complete", "verification_required", 403, true, 403},
		{"storage signature", "storage_put", "storage_upload_rejected", 401, false, 502},
		{"storage quota", "storage_put", "upload_rate_limited", 429, false, 429},
		{"init authentication", "init", "authentication_required", 401, false, 401},
		{"complete invalid JSON", "complete", "invalid_upload_response", 200, false, 502},
		{"complete processing failure", "complete", "upload_processing_failed", 422, false, 422},
		{"status challenge", "status", "verification_required", 403, true, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stages := []string{}
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				stage := ""
				switch {
				case strings.HasSuffix(r.URL.Path, "/init"):
					stage = "init"
				case strings.HasSuffix(r.URL.Path, "/complete"):
					stage = "complete"
				case strings.HasSuffix(r.URL.Path, "/status"):
					stage = "status"
				case r.URL.Host == "storage.googleapis.com":
					stage = "storage_put"
				default:
					t.Fatal("legacy fallback or unexpected endpoint")
				}
				stages = append(stages, stage)
				if stage == tc.stage {
					res := v2Response(r, tc.status, "SECRET signed URL https://private.invalid/?token=SECRET")
					if tc.kind == "upload_processing_failed" {
						res = v2Response(r, 200, `{"terminalError":"SECRET"}`)
					}
					if tc.challenge {
						res.Header.Set("Cf-Mitigated", "challenge")
						res.Header.Set("Content-Type", "text/html")
					}
					return res, nil
				}
				switch stage {
				case "init":
					return v2Response(r, 200, v2TestInit), nil
				case "storage_put":
					return v2Response(r, 200, ""), nil
				default:
					return v2Response(r, 200, `{"uploadId":"upload-1"}`), nil
				}
			})}
			media := NewMedia(client, "", "", "https://grok.com/rest/app-chat/upload-file-v2", "", 2*time.Second)
			_, _, err := media.Upload(context.Background(), accounts.Account{Token: "SECRET"}, "original.jpeg", "image/jpeg", "AAECAw==")
			d := MediaUploadDiagnostics(err)
			if err == nil || d["upload_error_kind"] != tc.kind || d["upload_stage"] != tc.stage {
				t.Fatalf("bad error: %v %v", err, d)
			}
			if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "private.invalid") {
				t.Fatal("sensitive error escaped")
			}
			var upstream *protocol.UpstreamError
			if !errors.As(err, &upstream) || upstream.Status != tc.wantStatus {
				t.Fatalf("bad effective status: %v", upstream)
			}
			expected := []string{"init", "storage_put", "complete", "status"}
			for n, stage := range expected {
				if stage == tc.stage {
					expected = expected[:n+1]
					break
				}
			}
			if !reflect.DeepEqual(stages, expected) {
				t.Fatalf("mutation retried or continued after error: %v", stages)
			}
		})
	}
}

func TestMediaUploadV2RejectsUnsafeStorageBeforeSending(t *testing.T) {
	for _, target := range []string{"http://storage.googleapis.com/file", "https://127.0.0.1/file", "https://internal.amazonaws.com/file", "https://storage.googleapis.com.evil.test/file", "https://user:secret@storage.googleapis.com/file", "https://storage.googleapis.com:444/file", "https://storage.googleapis.com/file#fragment"} {
		t.Run(target, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				raw, _ := json.Marshal(map[string]any{"uploadId": "u", "singlePut": map[string]any{"url": target}})
				return v2Response(r, 200, string(raw)), nil
			})}
			media := NewMedia(client, "", "", "https://grok.com/rest/app-chat/upload-file-v2", "", time.Second)
			_, _, err := media.Upload(context.Background(), accounts.Account{}, "original.jpeg", "image/jpeg", "AAECAw==")
			if calls != 1 || MediaUploadDiagnostics(err)["upload_error_kind"] != "invalid_upload_target" {
				t.Fatalf("unsafe target reached: calls=%d err=%v", calls, err)
			}
		})
	}
	for _, header := range []string{"Cookie", "Authorization", "Proxy-Authorization", "Host", "Content-Length"} {
		t.Run(header, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				raw, _ := json.Marshal(map[string]any{"uploadId": "u", "singlePut": map[string]any{"url": "https://storage.googleapis.com/file", "requiredHeaders": map[string]string{header: "SECRET"}}})
				return v2Response(r, 200, string(raw)), nil
			})}
			media := NewMedia(client, "", "", "https://grok.com/rest/app-chat/upload-file-v2", "", time.Second)
			_, _, err := media.Upload(context.Background(), accounts.Account{}, "original.jpeg", "image/jpeg", "AAECAw==")
			if calls != 1 || err == nil {
				t.Fatal("unsafe header reached storage")
			}
		})
	}
}

func TestMediaUploadV2RedirectAndCancellationDoNotResubmit(t *testing.T) {
	for _, mode := range []string{"redirect", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls > 1 {
					t.Fatal("unexpected second mutation")
				}
				if mode == "cancel" {
					cancel()
					return nil, context.Canceled
				}
				res := v2Response(r, 307, "")
				res.Header.Set("Location", "https://private.invalid")
				return res, nil
			})}
			media := NewMedia(client, "", "", "https://grok.com/rest/app-chat/upload-file-v2", "", time.Second)
			_, _, err := media.Upload(ctx, accounts.Account{}, "original.jpeg", "image/jpeg", "AAECAw==")
			if err == nil || calls != 1 {
				t.Fatal("failed upload was retried")
			}
		})
	}
	old := NewMedia(nil, "", "", "https://grok.com/rest/app-chat/upload-file", "", time.Second)
	if old.AssetUploadURL != "https://grok.com/rest/app-chat/upload-file-v2" {
		t.Fatal("old official default not migrated")
	}
}

package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"net/http"
	"testing"
)

func TestCapabilityRoutingImageExhaustedChatWorks(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			server, _ := multimodalTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintln(w, `data: {"message":{"author":{"role":"assistant"},"content":{"parts":["ok"]}}}`)
				fmt.Fprintln(w, "data: [DONE]")
			})
			rows, _ := server.store.AccountList()
			for _, row := range rows {
				_, _, e := server.store.UpdateAccount(accountToken(row), map[string]any{"status": "限流", "quota": 0, "limits_progress": []any{map[string]any{"feature_name": "image_gen", "remaining": 0}, map[string]any{"feature_name": "reason", "remaining": 2}}})
				if e != nil {
					t.Fatal(e)
				}
			}
			response := invokeMultimodal(server, path, multimodalRequestBody(t, path == "/v1/responses", false, false))
			if response.Code != 200 {
				t.Fatalf("%d %s", response.Code, response.Body.String())
			}
			rows, _ = server.store.AccountList()
			held := 0
			for _, row := range rows {
				b, _ := json.Marshal(row["quota_pending"])
				var q map[string]int
				_ = json.Unmarshal(b, &q)
				held += q["reason"]
				if q["image_gen"] != 0 {
					t.Fatal("text reserved image quota")
				}
			}
			if held != 1 {
				t.Fatalf("request quota not recorded: %d", held)
			}
			_, e := server.accountPool.ReserveIntent(context.Background(), []string{"basic"}, nil, isOpenAIAccount, 1, accounts.Intent{Kind: "image"})
			if e == nil {
				t.Fatal("image quota exhausted but image admitted")
			}
		})
	}
}

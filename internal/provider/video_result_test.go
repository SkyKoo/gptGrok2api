package provider

import (
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	"testing"
)

func TestVideoResultKeepsCandidateIdentity(t *testing.T) {
	for _, tc := range []struct {
		name             string
		events           []protocol.MediaEvent
		failed, complete bool
		url, post        string
	}{
		{"matching", []protocol.MediaEvent{{Kind: "video", VideoID: "v", ParentPostID: "parent", Progress: 20}, {Kind: "video", VideoID: "v", PostID: "post", Progress: 100, URL: "final"}}, false, true, "final", "post"},
		{"foreign parent", []protocol.MediaEvent{{Kind: "video", ParentPostID: "old-parent", Progress: 100, URL: "old"}}, true, false, "", ""},
		{"changed video", []protocol.MediaEvent{{Kind: "video", VideoID: "v1", Progress: 20}, {Kind: "video", VideoID: "v2", Progress: 100, URL: "wrong"}}, true, false, "", ""},
		{"other candidate cannot finish", []protocol.MediaEvent{{Kind: "video", VideoID: "v1", Progress: 20}, {Kind: "video", VideoID: "v2", SideBySideIndex: 1, Progress: 100, URL: "wrong"}}, false, false, "", "v1"},
		{"interleaved candidates", []protocol.MediaEvent{{Kind: "video", VideoID: "v1", Progress: 20}, {Kind: "video", VideoID: "v2", SideBySideIndex: 1, Progress: 100, URL: "wrong"}, {Kind: "video", VideoID: "v1", Progress: 100, URL: "right"}}, false, true, "right", "v1"},
		{"image cannot finish video", []protocol.MediaEvent{{Kind: "video", Progress: 20, URL: "preview"}, {Kind: "image", Progress: 100, URL: "image"}}, false, false, "", ""},
		{"preview is not final", []protocol.MediaEvent{{Kind: "video", Progress: 20, URL: "preview"}, {Kind: "video", Progress: 100}}, false, false, "", ""},
		{"empty result", []protocol.MediaEvent{{Kind: "video", Progress: 100}}, false, false, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := NewVideoResult("parent")
			var err error
			for _, e := range tc.events {
				if err = result.Observe(e); err != nil {
					break
				}
			}
			if (err != nil) != tc.failed {
				t.Fatalf("err=%v", err)
			}
			if err == nil && (result.Complete() != tc.complete || result.URL != tc.url || result.ResultPostID() != tc.post) {
				t.Fatalf("unexpected result: %+v", result)
			}
		})
	}
}

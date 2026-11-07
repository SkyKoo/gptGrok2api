package protocol

import "testing"

func TestParseImageAndVideoSSEEvents(t *testing.T) {
	image := ParseMediaData([]byte(`{"result":{"response":{"streamingImageGenerationResponse":{"imageUrl":"/images/a.png","progress":100}}}}`))
	if len(image) != 1 || image[0].Kind != "image" || image[0].Progress != 100 {
		t.Fatalf("unexpected image events: %#v", image)
	}
	video := ParseMediaData([]byte(`{"result":{"response":{"streamingVideoGenerationResponse":{"videoUrl":"/v.mp4","progress":100,"assetId":"v1"}}}}`))
	if len(video) != 1 || video[0].Kind != "video" || video[0].AssetID != "v1" {
		t.Fatalf("unexpected video events: %#v", video)
	}
}

func TestParseToolCalls(t *testing.T) {
	text := `<tool_calls><tool_call><tool_name>search</tool_name><parameters>{"query":"go"}</parameters></tool_call></tool_calls>`
	calls := ParseToolCalls(text, []string{"search"})
	if len(calls) != 1 || calls[0].Name != "search" || calls[0].Arguments != `{"query":"go"}` {
		t.Fatalf("unexpected tool calls: %#v", calls)
	}
}

func TestParseDataURI(t *testing.T) {
	filename, mime, encoded, err := ParseDataURI("data:image/png;base64,aGk=")
	if err != nil || filename != "file.png" || mime != "image/png" || encoded != "aGk=" {
		t.Fatalf("unexpected data URI result: %q %q %q %v", filename, mime, encoded, err)
	}
}

func TestVideoSegmentLengths(t *testing.T) {
	want := map[int][]int{2: {2}, 3: {3}, 4: {4}, 5: {5}, 6: {6}, 10: {10}, 15: {10, 5}}
	for seconds, expected := range want {
		got, ok := VideoSegmentLengths(seconds)
		if !ok || len(got) != len(expected) {
			t.Fatalf("unexpected segments for %d: %#v %v", seconds, got, ok)
		}
		for index := range expected {
			if got[index] != expected[index] {
				t.Fatalf("unexpected segments for %d: %#v", seconds, got)
			}
		}
	}
	for _, seconds := range []int{1, 7, 12, 16, 20} {
		if _, ok := VideoSegmentLengths(seconds); ok {
			t.Fatalf("unsupported video length %d accepted", seconds)
		}
	}
}

func TestResolveGeneratedVideoReference(t *testing.T) {
	got := ResolveGeneratedVideoReference("user/one", "post id")
	want := "https://assets.grok.com/users/user%2Fone/generated/post%20id/generated_video.mp4"
	if got != want {
		t.Fatalf("unexpected generated video URL: %q", got)
	}
	if ResolveGeneratedVideoReference("", "post") != "" || ResolveGeneratedVideoReference("user", "") != "" {
		t.Fatal("incomplete generated video references must be empty")
	}
}

func TestImagineImageSizeResolutionMapping(t *testing.T) {
	cases := []struct {
		size, aspect, resolution string
		ok                       bool
	}{
		{"auto", "1:1", "1k", true}, {"1024x1024", "1:1", "1k", true},
		{"1024x1792", "2:3", "1k", true}, {"1792x1024", "3:2", "1k", true},
		{"720x1280", "9:16", "1k", true}, {"1280x720", "16:9", "1k", true},
		{"2048x2048", "1:1", "2k", true}, {"2048x3072", "2:3", "2k", true},
		{"3072x2048", "3:2", "2k", true}, {"1440x2560", "9:16", "2k", true},
		{"2560x1440", "16:9", "2k", true}, {"3840x2160", "", "1k", false},
	}
	for _, tc := range cases {
		t.Run(tc.size, func(t *testing.T) {
			aspect, resolution, ok := ImagineImageSize(tc.size)
			if aspect != tc.aspect || resolution != tc.resolution || ok != tc.ok {
				t.Fatalf("got %q %q %v", aspect, resolution, ok)
			}
		})
	}
}

func TestVideoEventPreservesAssociationAndRejection(t *testing.T) {
	events := ParseMediaData([]byte(`{"result":{"response":{"streamingVideoGenerationResponse":{"videoId":"video","videoPostId":"post","parentPostId":"parent","sideBySideIndex":1,"rejectionReason":"VIDEO_IP","progress":100}}}}`))
	if len(events) != 1 {
		t.Fatal(events)
	}
	e := events[0]
	if e.VideoID != "video" || e.PostID != "post" || e.ParentPostID != "parent" || e.SideBySideIndex != 1 || e.RejectionReason != "VIDEO_IP" {
		t.Fatalf("lost correlation fields: %+v", e)
	}
	for _, payload := range []map[string]any{BuildVideoPayload("new prompt", "parent", "9:16", "720p", 6, "custom", nil), BuildVideoExtendPayload("new prompt", "parent", "result", "9:16", "720p", 5, "custom", 10)} {
		if payload["enableSideBySide"] != false {
			t.Fatal("single-result API enabled multiple candidates")
		}
	}
}

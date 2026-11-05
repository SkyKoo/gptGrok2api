package provider

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
)

func animationInt(n int) *int { return &n }
func tinyAnimation(t *testing.T, frames, delay int) []byte {
	t.Helper()
	a := &gif.GIF{LoopCount: 0}
	for i := 0; i < frames; i++ {
		frame := image.NewPaletted(image.Rect(0, 0, 2, 2), color.Palette{color.Black, color.White})
		frame.SetColorIndex(i%2, 0, 1)
		a.Image = append(a.Image, frame)
		a.Delay = append(a.Delay, delay)
		a.Disposal = append(a.Disposal, gif.DisposalBackground)
	}
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, a); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func TestGIFPreservesFramesAndRejectsStaticSubstitutes(t *testing.T) {
	raw := tinyAnimation(t, 9, 20)
	options := ImageOutputOptions{OutputFormat: "gif"}
	got, mime, err := options.apply(raw, "image/gif")
	if err != nil || mime != "image/gif" || !bytes.Equal(got, raw) {
		t.Fatalf("animation changed: %v", err)
	}
	if _, _, err = (ImageOutputOptions{}).apply(raw, "image/gif"); !IsImageOutputError(err) {
		t.Fatal("animated GIF silently flattened")
	}
	if _, _, err = options.apply(testImagePNG(t), "image/png"); !IsImageOutputError(err) {
		t.Fatal("static preview accepted")
	}
	for _, bad := range []ImageOutputOptions{{OutputFormat: "gif", FrameCount: animationInt(8)}, {OutputFormat: "gif", FrameDurationMS: animationInt(210)}} {
		if _, _, err := bad.apply(raw, "image/gif"); !IsImageOutputError(err) {
			t.Fatal("unfulfilled animation contract accepted")
		}
	}
	rounded := ImageOutputOptions{OutputFormat: "gif", FrameDurationMS: animationInt(25)}
	if _, _, err = rounded.apply(tinyAnimation(t, 9, 3), "image/gif"); err != nil {
		t.Fatal(err)
	}
}
func testImagePNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func TestGIFCompleteDecodeAndAllocationBounds(t *testing.T) {
	raw := tinyAnimation(t, 2, 20)
	// A valid first frame is not sufficient: the trailer or later frames may be missing.
	truncated := raw[:len(raw)-2]
	if _, _, err := image.Decode(bytes.NewReader(truncated)); err != nil {
		t.Fatal("fixture must have a decodable first frame", err)
	}
	if _, err := validatedImageMIME(truncated); err == nil {
		t.Fatal("truncated later frame accepted")
	}
	if _, err := decodeBoundedGIF(tinyAnimation(t, 65, 1)); err == nil {
		t.Fatal("unbounded frame count")
	}
	huge := append([]byte(nil), raw...)
	binary.LittleEndian.PutUint16(huge[6:8], 8000)
	binary.LittleEndian.PutUint16(huge[8:10], 8000)
	if _, err := decodeBoundedGIF(huge); err == nil {
		t.Fatal("aggregate canvas budget not enforced")
	}
	if _, err := decodeBoundedGIF(append(raw, 0)); err == nil {
		t.Fatal("trailing junk accepted")
	}
	for i := 0; i < len(raw); i++ {
		if _, err := decodeBoundedGIF(raw[:i]); err == nil {
			t.Fatalf("accepted truncated GIF at %d", i)
		}
	}
}
func TestAnimationValidation(t *testing.T) {
	for _, o := range []ImageOutputOptions{
		{FrameCount: animationInt(9)}, {FrameDurationMS: animationInt(200)},
		{OutputFormat: "gif", FrameCount: animationInt(0)}, {OutputFormat: "gif", FrameCount: animationInt(17)},
		{OutputFormat: "gif", FrameDurationMS: animationInt(0)}, {OutputFormat: "gif", FrameDurationMS: animationInt(2001)},
		{OutputFormat: "gif", Background: "transparent"}, {OutputFormat: "gif", Stream: true},
		{OutputFormat: "gif", PartialImages: animationInt(0)}, {OutputFormat: "gif", OutputCompression: animationInt(80)},
	} {
		if o.Validate() == nil {
			t.Fatalf("accepted unsupported animation options %+v", o)
		}
	}
	suffix := (ImageOutputOptions{OutputFormat: "gif"}).promptSuffix()
	if !strings.Contains(suffix, "9 frames") || !strings.Contains(suffix, "200 milliseconds") || strings.Contains(suffix, "Output file format: PNG") {
		t.Fatal("incorrect animation prompt")
	}
}
func TestAnimationAssetsExcludeSpritesheetAndSpeedVariants(t *testing.T) {
	raw, err := os.ReadFile("testdata/image26-animation.json")
	if err != nil {
		t.Fatal(err)
	}
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	id := ""
	refs := []string{}
	collectOpenAIGeneratedImageRefs(root, &id, &refs)
	refs = uniqueStrings(refs)
	if len(refs) != 1 || refs[0] != "sediment://animation-primary" {
		t.Fatalf("non-deliverable animation assets selected: %v", refs)
	}
}
func TestImage26EmptyConduitOnlyOnSuccessfulPrepare(t *testing.T) {
	for _, tc := range []struct {
		model, body string
		accepted    bool
	}{
		{"gpt-image-2.6-sunburst", `{"status":"ok","conduit_token":null}`, true},
		{"gpt-image-2.6-flare", `{"status":"ok","conduit_token":""}`, true},
		{"gpt-image-2", `{"status":"ok","conduit_token":null}`, false},
		{"gpt-image-2.6-sunburst", `{"status":"failed","conduit_token":null}`, false},
		{"gpt-image-2.6-sunburst", `{"status":"ok","error":{"code":"denied"}}`, false},
		{"gpt-image-2.6-sunburst", `{}`, false},
	} {
		t.Run(tc.model+tc.body, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, tc.body) }))
			defer srv.Close()
			client := NewOpenAIImage(srv.URL, srv.Client(), nil, time.Second)
			_, err := client.prepare(context.Background(), accounts.Account{Token: "test"}, openAIRequirements{}, "test", tc.model)
			if (err == nil) != tc.accepted {
				t.Fatalf("incorrect prepare result: %v", err)
			}
		})
	}
}

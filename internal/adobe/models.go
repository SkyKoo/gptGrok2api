package adobe

import (
	"bytes"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"strings"

	_ "golang.org/x/image/webp"
)

// Variant keeps a complete supported size/aspect/resolution combination together.
// Audio permits both silent and audio output; false permits only silent output.
type Variant struct {
	Size, AspectRatio, Resolution string
	Durations                     []int
	Audio                         bool
	AudioRequired, DefaultAudio   bool
	MaxReferenceImages            int
}

// Model describes the current Web Free route and its explicitly supported tiers.
// Default fields stay separate from Variants so old clients retain their defaults.
type Model struct {
	AudioRequired, DefaultAudio                                                   bool
	FeatureID                                                                     string
	ID, Name, OwnedBy, ModelID, ModelVersion, Kind, Size, Resolution, AspectRatio string
	Duration                                                                      int
	Variants                                                                      []Variant
	ReferenceUsage                                                                string
	ReferenceMIMEs                                                                []string
	MaxReferenceBytes                                                             int
}

func FreeModels() []Model {
	imageVariants := []Variant{}
	// Adobe's named aspect ratios use dimensions rounded for the model. Do not
	// infer these labels by reducing the pixel pair (1200x896 is the 4:3 tier).
	for _, p := range []struct{ size, aspect string }{
		{"1024x1024", "1:1"}, {"1264x848", "3:2"}, {"848x1264", "2:3"},
		{"1200x896", "4:3"}, {"896x1200", "3:4"}, {"1152x928", "5:4"},
		{"928x1152", "4:5"}, {"768x1376", "9:16"}, {"1376x768", "16:9"},
		{"1584x672", "21:9"},
	} {
		imageVariants = append(imageVariants, Variant{Size: p.size, AspectRatio: p.aspect, Resolution: "1K", MaxReferenceImages: 6})
	}
	return []Model{
		{ID: "gemini-3.1-flash-image", Name: "Gemini 3.1 Flash Image (Nano Banana 2)", OwnedBy: "google", ModelID: "gemini-flash", ModelVersion: "nano-banana-3", FeatureID: "firefly_3p:external:nano_banana_3", Kind: "image", Size: "1024x1024", AspectRatio: "1:1", Resolution: "1K", Variants: imageVariants, ReferenceUsage: "general", MaxReferenceBytes: 20 * 1024 * 1024, ReferenceMIMEs: []string{"image/png", "image/jpeg", "image/webp"}},
		{ID: "veo-3.1-lite", Name: "Veo 3.1 Lite", OwnedBy: "google", ModelID: "veo", ModelVersion: "3.1-lite-generate", FeatureID: "firefly_3p:external:veo_3_1_lite", Kind: "video", Size: "1920x1080", AspectRatio: "16:9", Resolution: "1080p", Duration: 8, Variants: []Variant{
			{Size: "1920x1080", AspectRatio: "16:9", Resolution: "1080p", MaxReferenceImages: 2, Durations: []int{8}, Audio: true},
			// Only these exact text-only, silent combinations passed the free preflight.
			{Size: "1920x1080", AspectRatio: "16:9", Resolution: "1080p", Durations: []int{4, 6}},
			{Size: "1280x720", AspectRatio: "16:9", Resolution: "720p", Durations: []int{8}},
			{Size: "720x1280", AspectRatio: "9:16", Resolution: "720p", Durations: []int{8}},
			{Size: "1080x1920", AspectRatio: "9:16", Resolution: "1080p", Durations: []int{8}},
		}, ReferenceUsage: "frame", MaxReferenceBytes: 20 * 1024 * 1024, ReferenceMIMEs: []string{"image/png", "image/jpeg"}},
		{ID: "firefly-image-4", Name: "Firefly Image 4", OwnedBy: "adobe", ModelID: "firefly-image", ModelVersion: "image4_standard", FeatureID: "firefly:clio_v4_diffusion", Kind: "image", Size: "2048x2048", AspectRatio: "1:1", Resolution: "2K", Variants: []Variant{{Size: "2048x2048", AspectRatio: "1:1", Resolution: "2K", MaxReferenceImages: 1}}, ReferenceUsage: "style", MaxReferenceBytes: 20 * 1024 * 1024, ReferenceMIMEs: []string{"image/png", "image/jpeg"}},
		{ID: "firefly-image-5", Name: "Firefly Image 5", OwnedBy: "adobe", ModelID: "firefly-image", ModelVersion: "image5", FeatureID: "firefly:internal:image5_generate", Kind: "image", Size: "1024x1024", AspectRatio: "1:1", Resolution: "1K", Variants: []Variant{{Size: "1024x1024", AspectRatio: "1:1", Resolution: "1K", MaxReferenceImages: 1}}, ReferenceUsage: "general", MaxReferenceBytes: 10 * 1024 * 1024, ReferenceMIMEs: []string{"image/png", "image/jpeg"}},
		{ID: "humain-image-1", Name: "HUMAIN Image 1", OwnedBy: "humain", ModelID: "humain", ModelVersion: "humain_image", FeatureID: "firefly:internal:foundry_humain_image", Kind: "image", Size: "2048x2048", AspectRatio: "1:1", Resolution: "2K", Variants: []Variant{
			{Size: "2048x2048", AspectRatio: "1:1", Resolution: "2K"},
			{Size: "2304x1792", AspectRatio: "4:3", Resolution: "2K"},
		}, ReferenceUsage: "", MaxReferenceBytes: 20 * 1024 * 1024, ReferenceMIMEs: []string{"image/png", "image/jpeg"}},
		{ID: "gemini-3.1-flash-image-2.1", Name: "Nano Banana 2.1", OwnedBy: "google", ModelID: "gemini-flash", ModelVersion: "nano-banana-2026-09", FeatureID: "firefly_3p:external:nano_banana_2026_09", Kind: "image", Size: "1024x1024", AspectRatio: "1:1", Resolution: "1K", Variants: []Variant{
			{Size: "1024x1024", AspectRatio: "1:1", Resolution: "1K", MaxReferenceImages: 14},
			// Named tiers use Adobe's official pixel map; reference support is unverified.
			{Size: "1200x896", AspectRatio: "4:3", Resolution: "1K"},
			{Size: "768x1376", AspectRatio: "9:16", Resolution: "1K"},
		}, ReferenceUsage: "general", MaxReferenceBytes: 20 * 1024 * 1024, ReferenceMIMEs: []string{"image/png", "image/jpeg", "image/webp"}},
		{ID: "gpt-image-2", Name: "GPT Image 2", OwnedBy: "openai", ModelID: "gpt-image", ModelVersion: "2", FeatureID: "firefly_3p:external:gpt_image_2", Kind: "image", Size: "1024x1024", AspectRatio: "1:1", Resolution: "1K", Variants: []Variant{{Size: "1024x1024", AspectRatio: "1:1", Resolution: "1K", MaxReferenceImages: 6}}, ReferenceUsage: "source", MaxReferenceBytes: 20 * 1024 * 1024, ReferenceMIMEs: []string{"image/png", "image/jpeg", "image/webp"}},
		{ID: "gpt-image-2.5-flare", Name: "GPT Image 2.5 Flare", OwnedBy: "openai", ModelID: "gpt-image", ModelVersion: "gpt-image-2.5-flare", FeatureID: "firefly_3p:external:gpt_image_2_5_flare", Kind: "image", Size: "1024x1024", AspectRatio: "1:1", Resolution: "1K", Variants: []Variant{{Size: "1024x1024", AspectRatio: "1:1", Resolution: "1K", MaxReferenceImages: 6}}, ReferenceUsage: "source", MaxReferenceBytes: 20 * 1024 * 1024, ReferenceMIMEs: []string{"image/png", "image/jpeg", "image/webp"}},
		{ID: "gpt-image-2.6-flare", Name: "GPT Image 2.6 Flare", OwnedBy: "openai", ModelID: "gpt-image", ModelVersion: "gpt-image-2.6-star-swirl", FeatureID: "firefly_3p:external:gpt_image_2_6_star_swirl", Kind: "image", Size: "1024x1024", AspectRatio: "1:1", Resolution: "1K", Variants: []Variant{{Size: "1024x1024", AspectRatio: "1:1", Resolution: "1K", MaxReferenceImages: 6}}, ReferenceUsage: "source", MaxReferenceBytes: 20 * 1024 * 1024, ReferenceMIMEs: []string{"image/png", "image/jpeg", "image/webp"}},
		{ID: "firefly-video", Name: "Firefly Video", OwnedBy: "adobe", ModelID: "firefly-video", ModelVersion: "clineto", FeatureID: "firefly:internal:video_generation_clineto_v1", Kind: "video", Size: "1920x1080", AspectRatio: "16:9", Resolution: "1080p", Duration: 5, Variants: []Variant{{Size: "1920x1080", AspectRatio: "16:9", Resolution: "1080p", MaxReferenceImages: 2, Durations: []int{5}}}, ReferenceUsage: "frame", MaxReferenceBytes: 20 * 1024 * 1024, ReferenceMIMEs: []string{"image/png", "image/jpeg"}},
		{ID: "gemini-omni-flash-1.1", Name: "Gemini Omni Flash 1.1", OwnedBy: "google", ModelID: "gemini-omni", ModelVersion: "omni-flash-1.1", FeatureID: "firefly_3p:external:gemini_omni_flash_1_1", Kind: "video", Size: "1280x720", AspectRatio: "16:9", Resolution: "720p", Duration: 5, AudioRequired: true, DefaultAudio: true, Variants: []Variant{{Size: "1280x720", AspectRatio: "16:9", Resolution: "720p", MaxReferenceImages: 2, Durations: []int{5}, Audio: true, AudioRequired: true, DefaultAudio: true}}, ReferenceUsage: "frame", MaxReferenceBytes: 20 * 1024 * 1024, ReferenceMIMEs: []string{"image/png", "image/jpeg"}},
		{ID: "gemini-omni-flash-1.2", Name: "Gemini Omni Flash 1.2", OwnedBy: "google", ModelID: "gemini-omni", ModelVersion: "omni-flash-1.2", FeatureID: "firefly_3p:external:gemini_omni_flash_1_2", Kind: "video", Size: "1280x720", AspectRatio: "16:9", Resolution: "720p", Duration: 5, AudioRequired: true, DefaultAudio: true, Variants: []Variant{{Size: "1280x720", AspectRatio: "16:9", Resolution: "720p", MaxReferenceImages: 2, Durations: []int{5}, Audio: true, AudioRequired: true, DefaultAudio: true}}, ReferenceUsage: "frame", MaxReferenceBytes: 20 * 1024 * 1024, ReferenceMIMEs: []string{"image/png", "image/jpeg"}},
		{ID: "veo-3.1-fast", Name: "Veo 3.1 Fast", OwnedBy: "google", ModelID: "veo", ModelVersion: "3.1-fast-generate", FeatureID: "firefly_3p:external:veo_3_fast", Kind: "video", Size: "1920x1080", AspectRatio: "16:9", Resolution: "1080p", Duration: 8, Variants: []Variant{
			// The free entitlement rejects audio, including the original 8-second tier.
			{Size: "1920x1080", AspectRatio: "16:9", Resolution: "1080p", MaxReferenceImages: 2, Durations: []int{8}},
			{Size: "1920x1080", AspectRatio: "16:9", Resolution: "1080p", Durations: []int{4, 6}},
			{Size: "1280x720", AspectRatio: "16:9", Resolution: "720p", Durations: []int{8}},
			{Size: "720x1280", AspectRatio: "9:16", Resolution: "720p", Durations: []int{8}},
			{Size: "1080x1920", AspectRatio: "9:16", Resolution: "1080p", Durations: []int{8}},
		}, ReferenceUsage: "frame", MaxReferenceBytes: 20 * 1024 * 1024, ReferenceMIMEs: []string{"image/png", "image/jpeg"}},
		{ID: "ray-3.14", Name: "Ray 3.14", OwnedBy: "luma", ModelID: "luma", ModelVersion: "3.14-ray", FeatureID: "firefly_3p:external:luma_ray_3_14", Kind: "video", Size: "1280x720", AspectRatio: "16:9", Resolution: "720p", Duration: 5, Variants: []Variant{
			{Size: "1280x720", AspectRatio: "16:9", Resolution: "720p", MaxReferenceImages: 2, Durations: []int{5}},
			{Size: "720x1280", AspectRatio: "9:16", Resolution: "720p", Durations: []int{5}},
		}, ReferenceUsage: "frame", MaxReferenceBytes: 10 * 1024 * 1024, ReferenceMIMEs: []string{"image/png", "image/jpeg"}},
	}
}
func FindModel(id string) (Model, bool) {
	for _, m := range FreeModels() {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

func (m Model) MatchImage(width, height, count, references int) (Variant, bool) {
	if m.Kind != "image" || count != 1 || references < 0 {
		return Variant{}, false
	}
	size := fmt.Sprintf("%dx%d", width, height)
	for _, v := range m.Variants {
		if size == v.Size && references <= v.MaxReferenceImages {
			return v, true
		}
	}
	return Variant{}, false
}
func (m Model) MatchVideo(width, height, duration int, resolution, aspect string, audio bool, references int) (Variant, bool) {
	if m.Kind != "video" || references < 0 {
		return Variant{}, false
	}
	size := fmt.Sprintf("%dx%d", width, height)
	for _, v := range m.Variants {
		if size != v.Size || resolution != v.Resolution || aspect != v.AspectRatio || audio && !v.Audio || !audio && v.AudioRequired || references > v.MaxReferenceImages {
			continue
		}
		for _, d := range v.Durations {
			if d == duration {
				return v, true
			}
		}
	}
	return Variant{}, false
}

// References are original validated image bytes. Only an uploaded Adobe handle
// is sent in the generation request; user-controlled URLs are never forwarded.
type ReferenceImage struct {
	Data []byte
	MIME string
}
type referenceBlob struct {
	ID    string `json:"id"`
	Usage string `json:"usage"`
	Order int    `json:"order,omitempty"`
}

func validateReferences(m Model, references []ReferenceImage) error {
	for _, r := range references {
		if len(r.Data) == 0 || len(r.Data) > m.MaxReferenceBytes {
			return &Error{Stage: "upload", Code: "invalid_reference_size"}
		}
		mime := http.DetectContentType(r.Data)
		if r.MIME != "" && r.MIME != mime {
			return &Error{Stage: "upload", Code: "reference_mime_mismatch"}
		}
		allowed := false
		for _, v := range m.ReferenceMIMEs {
			if v == mime {
				allowed = true
			}
		}
		if !allowed {
			return &Error{Stage: "upload", Code: "unsupported_reference_type"}
		}
		cfg, format, err := image.DecodeConfig(bytes.NewReader(r.Data))
		if err != nil || cfg.Width < 1 || cfg.Height < 1 || int64(cfg.Width)*int64(cfg.Height) > 40*1024*1024 ||
			mime != map[string]string{"jpeg": "image/jpeg", "png": "image/png", "webp": "image/webp"}[format] {
			return &Error{Stage: "upload", Code: "invalid_reference_image"}
		}
	}
	return nil
}
func ValidateImageRequest(r ImageRequest) (Model, Variant, error) {
	m, ok := FindModel(r.Model)
	v, fits := m.MatchImage(r.Width, r.Height, r.N, len(r.References))
	if !ok || !fits || strings.TrimSpace(r.Prompt) == "" || r.AspectRatio != "" && r.AspectRatio != v.AspectRatio || r.Resolution != "" && r.Resolution != v.Resolution {
		return Model{}, Variant{}, &Error{Stage: "submit", Code: "unsupported_free_parameters"}
	}
	if err := validateReferences(m, r.References); err != nil {
		return Model{}, Variant{}, err
	}
	return m, v, nil
}
func ValidateVideoRequest(r VideoRequest) (Model, Variant, error) {
	m, ok := FindModel(r.Model)
	v, fits := m.MatchVideo(r.Width, r.Height, r.Duration, r.Resolution, r.AspectRatio, r.GenerateAudio, len(r.References))
	if !ok || !fits || strings.TrimSpace(r.Prompt) == "" {
		return Model{}, Variant{}, &Error{Stage: "submit", Code: "unsupported_free_parameters"}
	}
	if err := validateReferences(m, r.References); err != nil {
		return Model{}, Variant{}, err
	}
	return m, v, nil
}

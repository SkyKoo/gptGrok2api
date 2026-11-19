package model

import (
	"slices"
	"testing"
)

func TestGPTImage2SupportsChatGPTAndAdobeRoutes(t *testing.T) {
	if got := MediaChannels("gpt-image-2"); !slices.Equal(got, []string{"chatgpt", "adobe"}) {
		t.Fatalf("unexpected gpt-image-2 channels: %v", got)
	}
	m, ok := AdobeModel("gpt-image-2")
	if !ok || m.ModelID != "gpt-image" || m.ModelVersion != "2" || m.Kind != "image" {
		t.Fatalf("missing Adobe gpt-image-2 mapping: ok=%t model=%+v", ok, m)
	}
	if _, ok := m.MatchImage(1024, 1024, 1, 2); !ok {
		t.Fatal("Adobe gpt-image-2 should accept a two-reference edit")
	}
}

func TestAdobeCapabilitiesPreserveExactParameterVariants(t *testing.T) {
	cap := MediaCapability("gemini-3.1-flash-image")
	if cap.Kind != "image" || cap.MaxReferenceImages != 6 || len(cap.Variants) != 10 {
		t.Fatalf("incorrect image capability: %+v", cap)
	}
	found := false
	for _, v := range cap.Variants {
		if len(v.Sizes) != 1 || len(v.AspectRatios) != 1 || len(v.Resolutions) != 1 {
			t.Fatal("variant unions may invent unsupported combinations")
		}
		if v.Sizes[0] == "1200x896" {
			found = v.AspectRatios[0] == "4:3" && v.Resolutions[0] == "1K"
		}
	}
	if !found {
		t.Fatal("official named ratio missing")
	}
	model, ok := Find(Catalog(), "gemini-3.1-flash-image")
	if !ok || model.OwnedBy != "google" || model.Capability&(Image|ImageEdit) != (Image|ImageEdit) {
		t.Fatal("reference model not exposed for editing")
	}
	video := MediaCapability("veo-3.1-lite")
	if video.Kind != "video" || !video.Audio || video.MaxReferenceImages != 2 {
		t.Fatalf("video frame/audio missing: %+v", video)
	}
}

func TestAdobePublicVideoVariantsPreservePerTierLimits(t *testing.T) {
	for _, id := range []string{"veo-3.1-lite", "veo-3.1-fast"} {
		cap := MediaCapability(id)
		if cap.Audio != (id == "veo-3.1-lite") || cap.DefaultAudio || cap.AudioRequired {
			t.Fatalf("%s reports incorrect audio capabilities: %+v", id, cap)
		}
		if len(cap.Variants) != 5 || !slices.Contains(cap.Resolutions, "720p") || !slices.Contains(cap.Durations, 4) || !slices.Contains(cap.Durations, 6) {
			t.Fatalf("%s expanded tiers are missing: %+v", id, cap)
		}
		for _, v := range cap.Variants {
			if len(v.Sizes) != 1 || len(v.Resolutions) != 1 || len(v.AspectRatios) != 1 {
				t.Fatalf("%s exposes an independent parameter union: %+v", id, v)
			}
			if v.MaxReferenceImages != 2 || v.Audio != (id == "veo-3.1-lite") || v.AudioRequired || v.DefaultAudio {
				t.Fatalf("%s tier differs from model-wide frame/audio limits: %+v", id, v)
			}
			if v.Sizes[0] != "1920x1080" && !slices.Equal(v.Durations, []int{8}) {
				t.Fatalf("%s exposed shorter duration at unverified dimensions: %+v", id, v)
			}
		}
	}
	ray := MediaCapability("ray-3.14")
	if ray.Audio || len(ray.Variants) != 3 || !slices.Equal(ray.Durations, []int{5, 10}) {
		t.Fatalf("Ray expanded capabilities incorrect: %+v", ray)
	}
	for _, v := range ray.Variants {
		if v.Sizes[0] == "720x1280" && (v.MaxReferenceImages != 0 || !slices.Equal(v.Durations, []int{5}) || !slices.Equal(v.AspectRatios, []string{"9:16"})) {
			t.Fatalf("Ray portrait tier lost its limits: %+v", v)
		}
		if slices.Contains(v.Durations, 10) && (!slices.Equal(v.Sizes, []string{"1280x720"}) || !slices.Equal(v.Resolutions, []string{"720p"}) || !slices.Equal(v.AspectRatios, []string{"16:9"}) || v.MaxReferenceImages != 0 || v.Audio || v.AudioRequired || v.DefaultAudio) {
			t.Fatalf("Ray 10 seconds leaked beyond its verified combination: %+v", v)
		}
	}
}

func TestAdobePublicImageVariantsPreserveOfficialSizeContracts(t *testing.T) {
	for _, tc := range []struct {
		id, size, aspect, resolution string
	}{
		{"gemini-3.1-flash-image-2.1", "1200x896", "4:3", "1K"},
		{"gemini-3.1-flash-image-2.1", "768x1376", "9:16", "1K"},
		{"humain-image-1", "2304x1792", "4:3", "2K"},
	} {
		cap := MediaCapability(tc.id)
		found := false
		for _, v := range cap.Variants {
			if slices.Equal(v.Sizes, []string{tc.size}) {
				found = true
				if !slices.Equal(v.AspectRatios, []string{tc.aspect}) || !slices.Equal(v.Resolutions, []string{tc.resolution}) || v.MaxReferenceImages != 0 || v.MaxCount != 1 {
					t.Fatalf("%s public image contract changed: %+v", tc.id, v)
				}
			}
		}
		if !found {
			t.Fatalf("%s missing official size %s", tc.id, tc.size)
		}
	}
}

func TestOmniPublicVariantsKeepDurationAudioAndReferenceBoundaries(t *testing.T) {
	for _, id := range []string{"gemini-omni-flash-1.1", "gemini-omni-flash-1.2"} {
		cap := MediaCapability(id)
		wantDurations := []int{5, 8}
		wantExtraDurations := []int{3, 4, 6, 7}
		if id == "gemini-omni-flash-1.1" {
			wantDurations = []int{5, 10}
			wantExtraDurations = []int{3, 4, 6, 7, 8, 9}
		}
		allDurations := append(slices.Clone(wantDurations), wantExtraDurations...)
		slices.Sort(allDurations)
		publicDurations := slices.Clone(cap.Durations)
		slices.Sort(publicDurations)
		if !cap.Audio || !cap.AudioRequired || !cap.DefaultAudio || len(cap.Variants) != 3 || !slices.Equal(cap.Resolutions, []string{"720p"}) || !slices.Equal(publicDurations, allDurations) {
			t.Fatalf("%s fixed-audio contract changed: %+v", id, cap)
		}
		portrait, landscape, extraDurations := false, false, false
		for _, v := range cap.Variants {
			if !v.Audio || !v.AudioRequired || !v.DefaultAudio || !slices.Equal(v.Resolutions, []string{"720p"}) {
				t.Fatalf("%s variant widened audio or output tiers: %+v", id, v)
			}
			if slices.Equal(v.Sizes, []string{"720x1280"}) {
				portrait = true
				if !slices.Equal(v.AspectRatios, []string{"9:16"}) || v.MaxReferenceImages != 0 || !slices.Equal(v.Durations, wantDurations) {
					t.Fatalf("%s portrait reference boundary changed: %+v", id, v)
				}
			} else {
				if !slices.Equal(v.Sizes, []string{"1280x720"}) || !slices.Equal(v.AspectRatios, []string{"16:9"}) {
					t.Fatalf("%s landscape contract changed: %+v", id, v)
				}
				if v.MaxReferenceImages == 2 && slices.Equal(v.Durations, wantDurations) {
					landscape = true
				} else if v.MaxReferenceImages == 0 && slices.Equal(v.Durations, wantExtraDurations) {
					extraDurations = true
				} else {
					t.Fatalf("%s extra durations inherited reference support: %+v", id, v)
				}
			}
		}
		if !portrait || !landscape || !extraDurations {
			t.Fatalf("%s missing distinct portrait, landscape or extra-duration capability", id)
		}
	}
}

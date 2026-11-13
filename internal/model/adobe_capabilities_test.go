package model

import (
	"slices"
	"testing"
)

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
			baseline := v.Sizes[0] == "1920x1080" && slices.Equal(v.Durations, []int{8})
			if baseline {
				if v.MaxReferenceImages != 2 || v.Audio != (id == "veo-3.1-lite") {
					t.Fatalf("%s baseline limits lost: %+v", id, v)
				}
			} else {
				audioAllowed := id == "veo-3.1-lite" && v.AspectRatios[0] == "9:16"
				if v.MaxReferenceImages != 0 || v.Audio != audioAllowed || v.AudioRequired || v.DefaultAudio {
					t.Fatalf("%s text-only tier differs from verified audio/input limits: %+v", id, v)
				}
			}
			if v.Sizes[0] != "1920x1080" && !slices.Equal(v.Durations, []int{8}) {
				t.Fatalf("%s exposed shorter duration at unverified dimensions: %+v", id, v)
			}
		}
	}
	ray := MediaCapability("ray-3.14")
	if ray.Audio || len(ray.Variants) != 2 {
		t.Fatalf("Ray expanded capabilities incorrect: %+v", ray)
	}
	for _, v := range ray.Variants {
		if v.Sizes[0] == "720x1280" && (v.MaxReferenceImages != 0 || !slices.Equal(v.Durations, []int{5}) || !slices.Equal(v.AspectRatios, []string{"9:16"})) {
			t.Fatalf("Ray portrait tier lost its limits: %+v", v)
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

func TestOmniPublicPortraitVariantKeepsFixedAudioAndReferenceBoundary(t *testing.T) {
	for _, id := range []string{"gemini-omni-flash-1.1", "gemini-omni-flash-1.2"} {
		cap := MediaCapability(id)
		if !cap.Audio || !cap.AudioRequired || !cap.DefaultAudio || len(cap.Variants) != 2 || !slices.Equal(cap.Resolutions, []string{"720p"}) || !slices.Equal(cap.Durations, []int{5}) {
			t.Fatalf("%s fixed-audio contract changed: %+v", id, cap)
		}
		portrait := false
		for _, v := range cap.Variants {
			if !v.Audio || !v.AudioRequired || !v.DefaultAudio || !slices.Equal(v.Resolutions, []string{"720p"}) || !slices.Equal(v.Durations, []int{5}) {
				t.Fatalf("%s variant widened audio or output tiers: %+v", id, v)
			}
			if slices.Equal(v.Sizes, []string{"720x1280"}) {
				portrait = true
				if !slices.Equal(v.AspectRatios, []string{"9:16"}) || v.MaxReferenceImages != 0 {
					t.Fatalf("%s portrait reference boundary changed: %+v", id, v)
				}
			} else if !slices.Equal(v.Sizes, []string{"1280x720"}) || !slices.Equal(v.AspectRatios, []string{"16:9"}) || v.MaxReferenceImages != 2 {
				t.Fatalf("%s original landscape contract changed: %+v", id, v)
			}
		}
		if !portrait {
			t.Fatalf("%s missing portrait capability", id)
		}
	}
}

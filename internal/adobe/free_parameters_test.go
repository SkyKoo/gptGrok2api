package adobe

import (
	"fmt"
	"testing"
)

func TestVerifiedVeoFreeCombinationsDoNotFormCartesianProduct(t *testing.T) {
	ref := referenceFixture(t, 2, 2)
	for _, id := range []string{"veo-3.1-lite", "veo-3.1-fast"} {
		for _, tier := range []struct {
			width, height, duration int
			resolution, aspect      string
		}{
			{1920, 1080, 4, "1080p", "16:9"},
			{1920, 1080, 6, "1080p", "16:9"},
			{1280, 720, 8, "720p", "16:9"},
			{720, 1280, 8, "720p", "9:16"},
			{1080, 1920, 8, "1080p", "9:16"},
		} {
			t.Run(fmt.Sprintf("%s/%dx%d/%ds", id, tier.width, tier.height, tier.duration), func(t *testing.T) {
				r := VideoRequest{Model: id, Prompt: "a landscape", Width: tier.width, Height: tier.height, Duration: tier.duration, Resolution: tier.resolution, AspectRatio: tier.aspect}
				if _, _, err := ValidateVideoRequest(r); err != nil {
					t.Fatalf("verified silent text request rejected: %v", err)
				}
				withAudio := r
				withAudio.GenerateAudio = true
				audioAllowed := id == "veo-3.1-lite" && tier.aspect == "9:16"
				if _, _, err := ValidateVideoRequest(withAudio); (err == nil) != audioAllowed {
					t.Fatalf("audio allowance differs from the verified tier: allowed=%t error=%v", audioAllowed, err)
				}
				for _, candidate := range []VideoRequest{r, withAudio} {
					candidate.References = []ReferenceImage{ref}
					if _, _, err := ValidateVideoRequest(candidate); err == nil {
						t.Fatal("new text-only tier inherited reference support")
					}
				}
			})
		}
		m, _ := FindModel(id)
		if m.Size != "1920x1080" || m.Duration != 8 || m.Resolution != "1080p" || m.DefaultAudio {
			t.Fatalf("%s changed an existing default", id)
		}
		if _, ok := m.MatchVideo(1920, 1080, 8, "1080p", "16:9", false, 2); !ok {
			t.Fatalf("%s lost the original reference tier", id)
		}
		if _, ok := m.MatchVideo(1920, 1080, 8, "1080p", "16:9", true, 2); ok != (id == "veo-3.1-lite") {
			t.Fatalf("%s audio entitlement does not match its free baseline", id)
		}
		for _, r := range []VideoRequest{
			{Width: 1280, Height: 720, Duration: 4, Resolution: "720p", AspectRatio: "16:9"},
			{Width: 720, Height: 1280, Duration: 6, Resolution: "720p", AspectRatio: "9:16"},
			{Width: 1080, Height: 1920, Duration: 4, Resolution: "1080p", AspectRatio: "9:16"},
			{Width: 1920, Height: 1080, Duration: 8, Resolution: "720p", AspectRatio: "16:9"},
			{Width: 1920, Height: 1080, Duration: 8, Resolution: "1080p", AspectRatio: "9:16"},
		} {
			r.Model, r.Prompt = id, "a landscape"
			if _, _, err := ValidateVideoRequest(r); err == nil {
				t.Fatalf("%s accepted an unverified combination: %+v", id, r)
			}
		}
	}
}

func TestRayPortraitFreeTierKeepsReferenceBoundary(t *testing.T) {
	m, _ := FindModel("ray-3.14")
	if _, ok := m.MatchVideo(720, 1280, 5, "720p", "9:16", false, 0); !ok {
		t.Fatal("verified portrait tier missing")
	}
	if _, ok := m.MatchVideo(720, 1280, 5, "720p", "9:16", false, 1); ok {
		t.Fatal("portrait tier inherited unverified reference support")
	}
	if _, ok := m.MatchVideo(720, 1280, 5, "720p", "9:16", true, 0); ok {
		t.Fatal("portrait tier permits unverified audio")
	}
	if _, ok := m.MatchVideo(1280, 720, 5, "720p", "16:9", false, 2); !ok {
		t.Fatal("original landscape reference tier lost")
	}
	if _, ok := m.MatchVideo(1080, 1920, 5, "1080p", "9:16", false, 0); ok {
		t.Fatal("portrait aspect enabled an unverified resolution")
	}
}

func TestOmniPortraitFreeTierRequiresAudioWithoutInheritingReferences(t *testing.T) {
	ref := referenceFixture(t, 2, 2)
	for _, id := range []string{"gemini-omni-flash-1.1", "gemini-omni-flash-1.2"} {
		t.Run(id, func(t *testing.T) {
			r := VideoRequest{Model: id, Prompt: "a landscape", Width: 720, Height: 1280, Duration: 5, Resolution: "720p", AspectRatio: "9:16", GenerateAudio: true}
			m, tier, err := ValidateVideoRequest(r)
			if err != nil || !tier.Audio || !tier.AudioRequired || !tier.DefaultAudio || tier.MaxReferenceImages != 0 {
				t.Fatalf("portrait fixed-audio contract lost: %+v, %v", tier, err)
			}
			if m.Size != "1280x720" || m.AspectRatio != "16:9" || m.Duration != 5 || !m.DefaultAudio || !m.AudioRequired {
				t.Fatalf("existing defaults changed: %+v", m)
			}
			if _, ok := m.MatchVideo(1280, 720, 5, "720p", "16:9", true, 2); !ok {
				t.Fatal("original landscape reference tier lost")
			}
			for _, tc := range []struct {
				name   string
				mutate func(*VideoRequest)
			}{
				{"silent", func(r *VideoRequest) { r.GenerateAudio = false }},
				{"reference", func(r *VideoRequest) { r.References = []ReferenceImage{ref} }},
				{"1080p", func(r *VideoRequest) { r.Width, r.Height, r.Resolution = 1080, 1920, "1080p" }},
				{"square", func(r *VideoRequest) { r.Width, r.Height, r.AspectRatio = 720, 720, "1:1" }},
				{"mismatched-aspect", func(r *VideoRequest) { r.AspectRatio = "16:9" }},
				{"duration", func(r *VideoRequest) { r.Duration = 8 }},
			} {
				t.Run(tc.name, func(t *testing.T) {
					invalid := r
					tc.mutate(&invalid)
					if _, _, err := ValidateVideoRequest(invalid); err == nil {
						t.Fatalf("unverified combination accepted: %+v", invalid)
					}
				})
			}
		})
	}
}

func TestNewImageFreeTiersUseOfficialDimensionsWithoutInheritedReferences(t *testing.T) {
	ref := referenceFixture(t, 2, 2)
	for _, tc := range []struct {
		id                 string
		width, height      int
		aspect, resolution string
	}{
		{"gemini-3.1-flash-image-2.1", 1200, 896, "4:3", "1K"},
		{"gemini-3.1-flash-image-2.1", 768, 1376, "9:16", "1K"},
		{"humain-image-1", 2304, 1792, "4:3", "2K"},
	} {
		t.Run(tc.id+"/"+tc.aspect, func(t *testing.T) {
			r := ImageRequest{Model: tc.id, Prompt: "a landscape", Width: tc.width, Height: tc.height, N: 1}
			_, tier, err := ValidateImageRequest(r)
			if err != nil || tier.AspectRatio != tc.aspect || tier.Resolution != tc.resolution {
				t.Fatalf("official named tier lost: %+v, %v", tier, err)
			}
			r.AspectRatio, r.Resolution = tc.aspect, tc.resolution
			if _, _, err := ValidateImageRequest(r); err != nil {
				t.Fatalf("explicit official dimensions rejected: %v", err)
			}
			withReference := r
			withReference.References = []ReferenceImage{ref}
			if _, _, err := ValidateImageRequest(withReference); err == nil {
				t.Fatal("new text-only image tier inherited reference support")
			}
			wrongResolution := r
			wrongResolution.Resolution = "4K"
			if _, _, err := ValidateImageRequest(wrongResolution); err == nil {
				t.Fatal("verified aspect enabled an unverified resolution")
			}
		})
	}
	nano21, _ := FindModel("gemini-3.1-flash-image-2.1")
	if _, ok := nano21.MatchImage(1024, 1024, 1, 14); !ok {
		t.Fatal("Nano Banana 2.1 square reference limit changed")
	}
	for _, id := range []string{"gemini-3.1-flash-image", "gemini-3.1-flash-image-2.1"} {
		m, _ := FindModel(id)
		for _, side := range []int{2048, 4096} {
			if _, ok := m.MatchImage(side, side, 1, 0); ok {
				t.Fatalf("%s accepted an explicitly denied %d tier", id, side)
			}
		}
	}
}

package model

import "testing"

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

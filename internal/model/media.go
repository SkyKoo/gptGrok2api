package model

import (
	"github.com/auucoder/gptgrok2api-go/internal/adobe"
	"strings"
)

// MediaVariant describes one complete supported parameter combination. A union
// of independent dropdowns is not sufficient to decide whether a route fits.
type MediaVariant struct {
	Sizes              []string `json:"sizes"`
	Qualities          []string `json:"qualities,omitempty"`
	AspectRatios       []string `json:"aspect_ratios,omitempty"`
	Resolutions        []string `json:"resolutions,omitempty"`
	Durations          []int    `json:"durations,omitempty"`
	AudioRequired      bool     `json:"audio_required"`
	DefaultAudio       bool     `json:"default_audio"`
	Audio              bool     `json:"audio"`
	MaxReferenceImages int      `json:"max_reference_images"`
	OutputFormats      []string `json:"output_formats,omitempty"`
	MaxCount           int      `json:"max_count"`
	Mask               bool     `json:"mask"`
	Transparent        bool     `json:"transparent"`
}

type MediaCapabilities struct {
	Qualities          []string       `json:"qualities,omitempty"`
	Kind               string         `json:"kind"`
	Variants           []MediaVariant `json:"variants"`
	Sizes              []string       `json:"sizes"`
	AspectRatios       []string       `json:"aspect_ratios,omitempty"`
	Resolutions        []string       `json:"resolutions,omitempty"`
	Durations          []int          `json:"durations,omitempty"`
	AudioRequired      bool           `json:"audio_required"`
	DefaultAudio       bool           `json:"default_audio"`
	Audio              bool           `json:"audio"`
	MaxReferenceImages int            `json:"max_reference_images"`
	OutputFormats      []string       `json:"output_formats,omitempty"`
}

func AdobeModel(id string) (adobe.Model, bool) {
	for _, m := range adobe.FreeModels() {
		if m.ID == id {
			return m, true
		}
	}
	return adobe.Model{}, false
}

func MediaChannels(id string) []string {
	out := []string{}
	if strings.HasPrefix(id, "gpt-image-") {
		out = append(out, "chatgpt")
	}
	if strings.HasPrefix(id, "grok-imagine-") {
		out = append(out, "grok")
	}
	if _, ok := AdobeModel(id); ok {
		out = append(out, "adobe")
	}
	return out
}

func ChannelVariants(id, channel string) []MediaVariant {
	if channel == "adobe" {
		if m, ok := AdobeModel(id); ok {
			out := make([]MediaVariant, 0, len(m.Variants))
			for _, tier := range m.Variants {
				v := MediaVariant{Qualities: []string{"auto"}, Sizes: []string{tier.Size}, AspectRatios: []string{tier.AspectRatio}, Resolutions: []string{tier.Resolution}, Audio: tier.Audio, AudioRequired: tier.AudioRequired, DefaultAudio: tier.DefaultAudio, MaxReferenceImages: tier.MaxReferenceImages, MaxCount: 1, OutputFormats: []string{"png"}}
				if m.Kind == "video" {
					v.Durations = append([]int(nil), tier.Durations...)
					v.OutputFormats = nil
				}
				out = append(out, v)
			}
			return out
		}
		return nil
	}
	if channel == "chatgpt" {
		formats := []string{"png", "jpeg", "webp"}
		if strings.HasPrefix(id, "gpt-image-2.6-") {
			formats = append(formats, "gif")
		}
		return []MediaVariant{{Qualities: []string{"auto", "low", "medium", "high"}, Sizes: []string{"auto", "1024x1024", "1536x1024", "1024x1536", "1792x1024", "1024x1792"}, MaxReferenceImages: 7, MaxCount: 10, OutputFormats: formats, Mask: true, Transparent: true}}
	}
	if channel == "grok" {
		if id == "grok-imagine-image-edit" {
			return []MediaVariant{{Sizes: []string{"1024x1024"}, MaxReferenceImages: 5, MaxCount: 2}}
		}
		if id == "grok-imagine-video" {
			return []MediaVariant{{Sizes: []string{"720x1280", "1280x720", "1024x1024", "1024x1792", "1792x1024"}, AspectRatios: []string{"9:16", "16:9", "1:1", "2:3", "3:2"}, Resolutions: []string{"480p", "720p"}, Durations: []int{1, 2, 3, 4, 5, 6}, MaxReferenceImages: 7, MaxCount: 1}}
		}
		return []MediaVariant{{Sizes: []string{"1024x1024", "1024x1792", "1792x1024", "1536x1024", "1024x1536"}, MaxReferenceImages: 5, MaxCount: 10}}
	}
	return nil
}

func MediaCapability(id string) MediaCapabilities {
	c := MediaCapabilities{Kind: "image", Variants: []MediaVariant{}, Sizes: []string{}}
	if m, ok := AdobeModel(id); ok {
		c.Kind = m.Kind
		c.AudioRequired, c.DefaultAudio = m.AudioRequired, m.DefaultAudio
	} else if strings.Contains(id, "video") {
		c.Kind = "video"
	}
	add := func(dst *[]string, values []string) {
		for _, s := range values {
			found := false
			for _, old := range *dst {
				if old == s {
					found = true
				}
			}
			if !found {
				*dst = append(*dst, s)
			}
		}
	}
	allAudioRequired, sawVariant := true, false
	for _, channel := range MediaChannels(id) {
		for _, v := range ChannelVariants(id, channel) {
			sawVariant = true
			allAudioRequired = allAudioRequired && v.AudioRequired
			c.Variants = append(c.Variants, v)
			add(&c.Qualities, v.Qualities)
			add(&c.Sizes, v.Sizes)
			add(&c.AspectRatios, v.AspectRatios)
			add(&c.Resolutions, v.Resolutions)
			add(&c.OutputFormats, v.OutputFormats)
			for _, d := range v.Durations {
				found := false
				for _, old := range c.Durations {
					if old == d {
						found = true
					}
				}
				if !found {
					c.Durations = append(c.Durations, d)
				}
			}
			c.Audio = c.Audio || v.Audio
			if v.MaxReferenceImages > c.MaxReferenceImages {
				c.MaxReferenceImages = v.MaxReferenceImages
			}
		}
	}
	c.AudioRequired = sawVariant && allAudioRequired
	return c
}

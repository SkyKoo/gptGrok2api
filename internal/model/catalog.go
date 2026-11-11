package model

import (
	"github.com/auucoder/gptgrok2api-go/internal/adobe"
	"time"
)

type Capability uint8

const (
	Chat Capability = 1 << iota
	Image
	ImageEdit
	Video
	ConsoleChat
)

type Spec struct {
	ID         string
	Name       string
	OwnedBy    string
	Created    int64
	Capability Capability
	Enabled    bool
}

func (s Spec) Public() map[string]any {
	value := map[string]any{
		"id":       s.ID,
		"object":   "model",
		"created":  s.Created,
		"owned_by": s.OwnedBy,
		"name":     s.Name,
	}
	if len(MediaChannels(s.ID)) > 0 {
		value["media_capabilities"] = MediaCapability(s.ID)
	}
	return value
}

func Catalog() []Spec {
	created := time.Now().Unix()
	items := []Spec{
		{"grok-web-auto", "Grok Web · 自动", "xai", created, Chat, true},
		{"grok-web-fast", "Grok Web · 快速", "xai", created, Chat, true},
		{"grok-web-expert", "Grok Web · 专家", "xai", created, Chat, true},
		{"grok-web-heavy", "Grok Web · 重型", "xai", created, Chat, true},
		{"gpt-image-2", "GPT Image 2", "openai-compatible", created, Image, true},
		{"gpt-image-2.5-sunburst", "GPT Image 2.5 Sunburst", "openai-compatible", created, Image, true},
		{"gpt-image-2.5-flare", "GPT Image 2.5 Flare", "openai-compatible", created, Image, true},
		{"gpt-image-2.6-sunburst", "GPT Image 2.6 Sunburst", "openai-compatible", created, Image, true},
		{"gpt-image-2.6-flare", "GPT Image 2.6 Flare", "openai-compatible", created, Image, true},
		{"grok-imagine-image-pro", "Grok Imagine Image Pro", "xai", created, Image, true},
		{"grok-imagine-image-edit", "Grok Imagine Image Edit", "xai", created, ImageEdit, true},
		{"grok-imagine-video", "Grok Imagine Video", "xai", created, Video, true},
		{"grok-4.3-console", "Grok 4.3 (Console)", "xai", created, ConsoleChat, true},
		{"grok-4.3-low", "Grok 4.3 Low Thinking", "xai", created, ConsoleChat, true},
		{"grok-4.3-medium", "Grok 4.3 Medium Thinking", "xai", created, ConsoleChat, true},
		{"grok-4.3-high", "Grok 4.3 High Thinking", "xai", created, ConsoleChat, true},
		{"grok-4.20-0309-reasoning-console", "Grok 4.20 0309 Reasoning (Console)", "xai", created, ConsoleChat, true},
		{"grok-4.20-0309-console", "Grok 4.20 0309 (Console)", "xai", created, ConsoleChat, true},
		{"grok-4.20-multi-agent-console", "Grok 4.20 Multi-Agent (Console)", "xai", created, ConsoleChat, true},
		{"grok-4.20-multi-agent-low", "Grok 4.20 Multi-Agent Low", "xai", created, ConsoleChat, true},
		{"grok-4.20-multi-agent-medium", "Grok 4.20 Multi-Agent Medium", "xai", created, ConsoleChat, true},
		{"grok-4.20-multi-agent-high", "Grok 4.20 Multi-Agent High", "xai", created, ConsoleChat, true},
		{"grok-4.20-multi-agent-xhigh", "Grok 4.20 Multi-Agent XHigh", "xai", created, ConsoleChat, true},
		{"grok-4.20-0309-non-reasoning-console", "Grok 4.20 0309 Non-Reasoning (Console)", "xai", created, ConsoleChat, true},
		{"grok-build-console", "Grok Build 0.1 (Console)", "xai", created, ConsoleChat, true},
	}
	for _, m := range adobe.FreeModels() {
		found := false
		for i, item := range items {
			if item.ID == m.ID {
				for _, v := range m.Variants {
					if m.Kind == "image" && v.MaxReferenceImages > 0 {
						items[i].Capability |= ImageEdit
					}
				}
				found = true
				break
			}
		}
		if !found {
			capability := Image
			if m.Kind == "video" {
				capability = Video
			} else {
				for _, v := range m.Variants {
					if v.MaxReferenceImages > 0 {
						capability |= ImageEdit
					}
				}
			}
			items = append(items, Spec{m.ID, m.Name, m.OwnedBy, created, capability, true})
		}
	}
	return items
}

// CanonicalID keeps old clients on the supported high-quality image route.
func CanonicalID(id string) string {
	if mode, ok := ResolveGrokWebMode(id); ok {
		return GrokWebID(mode)
	}
	switch id {
	case "grok-imagine-image", "grok-imagine-image-lite":
		return "grok-imagine-image-pro"
	}
	return id
}
func Find(items []Spec, id string) (Spec, bool) {
	id = CanonicalID(id)
	for _, item := range items {
		if item.Enabled && item.ID == id {
			return item, true
		}
	}
	return Spec{}, false
}

package model

import "testing"

func TestCatalogContainsCoreModels(t *testing.T) {
	items := Catalog()
	for _, id := range []string{"grok-4.20-fast", "grok-imagine-image", "grok-imagine-video"} {
		if _, ok := Find(items, id); !ok {
			t.Fatalf("catalog missing %q", id)
		}
	}
}

func TestImageModelChatCompatibilityRoute(t *testing.T) {
	route, ok := ResolveChat("gpt-image-2")
	if !ok || !route.OpenAI || !route.Image {
		t.Fatal("gpt-image-2 must retain its image chat-completions compatibility route")
	}
	for _, item := range Catalog() {
		if item.ID == "gpt-image-2" && item.Capability&Chat != 0 {
			t.Fatal("gpt-image-2 must remain hidden from the normal chat catalog")
		}
	}
}

func TestCatalogChatModelsHaveRoutes(t *testing.T) {
	for _, item := range Catalog() {
		if item.Capability&(Chat|ConsoleChat) == 0 {
			continue
		}
		route, ok := ResolveChat(item.ID)
		if !ok {
			t.Errorf("catalog chat model %q has no route", item.ID)
			continue
		}
		wantConsole := item.Capability&ConsoleChat != 0
		if route.Console != wantConsole {
			t.Errorf("catalog chat model %q console=%v, want %v", item.ID, route.Console, wantConsole)
		}
		if route.Provider != "" || route.OpenAI {
			t.Errorf("catalog Grok chat model %q unexpectedly routes to provider=%q openai=%v", item.ID, route.Provider, route.OpenAI)
		}
	}
}

func TestConsoleThinkingAliasesUseNativeRoute(t *testing.T) {
	for _, id := range []string{
		"grok-4.3-low", "grok-4.3-medium", "grok-4.3-high",
		"grok-4.20-multi-agent-low", "grok-4.20-multi-agent-medium",
		"grok-4.20-multi-agent-high", "grok-4.20-multi-agent-xhigh",
	} {
		route, ok := ResolveChat(id)
		if !ok || !route.Console || route.Mode != "console" {
			t.Errorf("%s must use native console route: %#v, ok=%v", id, route, ok)
		}
	}
}

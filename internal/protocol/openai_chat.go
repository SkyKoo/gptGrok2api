package protocol

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const OpenAIChatMaxImages = 7

type InputError struct{ Param, Message string }

func (e *InputError) Error() string            { return e.Message }
func invalidInput(param, message string) error { return &InputError{Param: param, Message: message} }

// NormalizeOpenAIChat validates the supported ChatGPT Web subset. Image bytes
// are resolved later by the HTTP layer, before an account lease is acquired.
func NormalizeOpenAIChat(request ChatRequest) (ChatRequest, error) {
	if err := validateOpenAIParameters(request.Fields, false); err != nil {
		return request, err
	}
	if request.Temperature != nil || request.TopP != nil || request.MaxTokens != nil {
		return request, invalidInput("temperature/top_p/max_tokens", "ChatGPT Web does not support explicit sampling or token limits")
	}
	if len(request.Tools) != 0 || !openAITextToolChoice(request.ToolChoice) {
		return request, invalidInput("tools", "ChatGPT Web function tools are not supported")
	}
	if request.Size != "" {
		return request, invalidInput("size", "size is only supported for image generation")
	}
	if request.ReasoningEffort != "" {
		switch request.ReasoningEffort {
		case "none", "minimal", "low", "medium", "high", "xhigh":
		default:
			return request, invalidInput("reasoning_effort", "unsupported reasoning effort")
		}
	}
	messages, err := normalizeOpenAIMessages(request.Messages, false)
	request.Messages = messages
	return request, err
}

// OpenAIResponsesChat translates only text/image messages, including assistant
// history. Stateful IDs, function calls and files require separate adapters.
func OpenAIResponsesChat(request ResponsesRequest) (ChatRequest, error) {
	if err := validateOpenAIParameters(request.Fields, true); err != nil {
		return ChatRequest{}, err
	}
	if len(request.Tools) != 0 || !openAITextToolChoice(request.ToolChoice) {
		return ChatRequest{}, invalidInput("tools", "ChatGPT Web function tools are not supported")
	}
	messages := []Message{}
	switch input := request.Input.(type) {
	case string:
		if strings.TrimSpace(input) != "" {
			messages = append(messages, Message{Role: "user", Content: input})
		}
	case []any:
		for index, raw := range input {
			item, ok := raw.(map[string]any)
			if !ok {
				return ChatRequest{}, invalidInput(fmt.Sprintf("input[%d]", index), "input items must be message objects")
			}
			kind, validKind := item["type"].(string)
			if item["type"] != nil && !validKind {
				return ChatRequest{}, invalidInput(fmt.Sprintf("input[%d].type", index), "type must be a string")
			}
			if kind != "" && kind != "message" {
				return ChatRequest{}, invalidInput(fmt.Sprintf("input[%d].type", index), "only text/image message input items are supported")
			}
			if item["tool_calls"] != nil {
				return ChatRequest{}, invalidInput(fmt.Sprintf("input[%d].tool_calls", index), "tool call history is not supported")
			}
			role, _ := item["role"].(string)
			messages = append(messages, Message{Role: role, Content: item["content"]})
		}
	default:
		return ChatRequest{}, invalidInput("input", "input must be text or an array of messages")
	}
	if len(messages) == 0 {
		return ChatRequest{}, invalidInput("input", "input cannot be empty")
	}
	normalized, err := normalizeOpenAIMessages(messages, true)
	if err != nil {
		return ChatRequest{}, err
	}
	if strings.TrimSpace(request.Instructions) != "" {
		normalized = append([]Message{{Role: "developer", Content: request.Instructions}}, normalized...)
	}
	effort, validEffort := request.Reasoning["effort"].(string)
	if request.Reasoning["effort"] != nil && !validEffort {
		return ChatRequest{}, invalidInput("reasoning.effort", "effort must be a string")
	}
	for key := range request.Reasoning {
		if key != "effort" {
			return ChatRequest{}, invalidInput("reasoning."+key, "this reasoning option is not supported")
		}
	}
	chat := ChatRequest{Model: request.Model, Messages: normalized, Stream: request.Stream, ReasoningEffort: effort, Temperature: request.Temperature, TopP: request.TopP, MaxTokens: request.MaxOutputTokens}
	result, err := NormalizeOpenAIChat(chat)
	if invalid, ok := err.(*InputError); ok && invalid.Param == "reasoning_effort" {
		invalid.Param = "reasoning.effort"
	}
	return result, err
}

func openAITextToolChoice(value any) bool {
	if value == nil {
		return true
	}
	text, ok := value.(string)
	return ok && text == "none"
}

func normalizeOpenAIMessages(messages []Message, responses bool) ([]Message, error) {
	if len(messages) == 0 {
		return nil, invalidInput("messages", "messages cannot be empty")
	}
	result := make([]Message, 0, len(messages))
	imageCount := 0
	for index, message := range messages {
		param := fmt.Sprintf("messages[%d]", index)
		if responses {
			param = fmt.Sprintf("input[%d]", index)
		}
		role := strings.ToLower(strings.TrimSpace(message.Role))
		switch role {
		case "system", "developer", "user", "assistant":
		default:
			return nil, invalidInput(param+".role", "unsupported message role")
		}
		if len(message.ToolCalls) != 0 {
			return nil, invalidInput(param+".tool_calls", "tool call history is not supported")
		}
		parts := []any{}
		meaningful := false
		switch content := message.Content.(type) {
		case string:
			if strings.TrimSpace(content) != "" {
				parts = append(parts, map[string]any{"type": "text", "text": content})
				meaningful = true
			}
		case []any:
			for partIndex, raw := range content {
				partParam := fmt.Sprintf("%s.content[%d]", param, partIndex)
				part, ok := raw.(map[string]any)
				if !ok {
					return nil, invalidInput(partParam, "content parts must be objects")
				}
				kind, _ := part["type"].(string)
				isText := kind == "text" && !responses || responses && (kind == "input_text" || kind == "output_text" && role == "assistant")
				if isText {
					text, ok := part["text"].(string)
					if !ok {
						return nil, invalidInput(partParam+".text", "text must be a string")
					}
					parts = append(parts, map[string]any{"type": "text", "text": text})
					meaningful = meaningful || strings.TrimSpace(text) != ""
					continue
				}
				if !(kind == "image_url" && !responses || kind == "input_image" && responses) {
					return nil, invalidInput(partParam+".type", "only text and image URL content parts are supported")
				}
				if role != "user" {
					return nil, invalidInput(partParam, "image input is only supported in user messages")
				}
				if part["file_id"] != nil {
					return nil, invalidInput(partParam+".file_id", "file_id is not supported; use image_url or a base64 data URL")
				}
				var url, detail string
				var rawDetail any
				if responses {
					url, _ = part["image_url"].(string)
					rawDetail = part["detail"]
					detail, _ = rawDetail.(string)
				} else {
					value, ok := part["image_url"].(map[string]any)
					if !ok {
						return nil, invalidInput(partParam+".image_url", "image_url must be an object containing url")
					}
					url, _ = value["url"].(string)
					rawDetail = value["detail"]
					detail, _ = rawDetail.(string)
				}
				if rawDetail != nil {
					if _, ok := rawDetail.(string); !ok {
						return nil, invalidInput(partParam+".detail", "detail must be a string")
					}
				}
				if detail != "" && detail != "auto" {
					return nil, invalidInput(partParam+".detail", "ChatGPT Web supports only automatic image detail")
				}
				url = strings.TrimSpace(url)
				if !(strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "data:image/")) {
					return nil, invalidInput(partParam+".image_url", "image input must use an HTTP(S) URL or image data URL")
				}
				imageCount++
				meaningful = true
				if imageCount > OpenAIChatMaxImages {
					return nil, invalidInput("messages", "at most 7 images are supported per chat request")
				}
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
			}
		default:
			return nil, invalidInput(param+".content", "message content must be text or an array of text/image parts")
		}
		if !meaningful {
			return nil, invalidInput(param+".content", "message content cannot be empty")
		}
		result = append(result, Message{Role: role, Content: parts})
	}
	return result, nil
}

func validateOpenAIParameters(fields map[string]json.RawMessage, responses bool) error {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		raw := json.RawMessage(strings.TrimSpace(string(fields[key])))
		if strings.TrimSpace(string(raw)) == "null" {
			continue
		}
		switch key {
		case "model", "stream", "tools", "tool_choice":
			continue
		case "messages", "reasoning_effort":
			if !responses {
				continue
			}
		case "input", "instructions", "reasoning":
			if responses {
				continue
			}
		case "store", "background":
			if string(raw) == "false" {
				continue
			}
		case "n":
			if !responses && string(raw) == "1" {
				continue
			}
		case "include":
			if responses {
				var v []any
				if json.Unmarshal(raw, &v) == nil && len(v) == 0 {
					continue
				}
			}
		case "response_format":
			if !responses {
				var v map[string]any
				if json.Unmarshal(raw, &v) == nil && len(v) == 1 && isTextFormat(v["type"]) {
					continue
				}
			}
		case "text":
			if responses {
				var v struct {
					Format map[string]any `json:"format"`
				}
				var all map[string]any
				if json.Unmarshal(raw, &v) == nil && json.Unmarshal(raw, &all) == nil && len(all) == 1 && len(v.Format) == 1 && isTextFormat(v.Format["type"]) {
					continue
				}
			}
		}
		return invalidInput(key, "parameter is not supported by the ChatGPT Web adapter: "+key)
	}
	return nil
}

func isTextFormat(v any) bool { text, ok := v.(string); return ok && text == "text" }

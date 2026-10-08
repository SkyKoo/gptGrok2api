package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/model"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

func responseTextPart(text string) map[string]any {
	return map[string]any{"type": "output_text", "text": text, "annotations": []any{}, "logprobs": []any{}}
}

func responseTextItem(id, status, text string, withPart bool) map[string]any {
	content := []any{}
	if withPart {
		content = append(content, responseTextPart(text))
	}
	return map[string]any{"id": id, "type": "message", "role": "assistant", "status": status, "content": content}
}

func (s *Server) openAIResponses(w http.ResponseWriter, r *http.Request, input protocol.ResponsesRequest, route model.ChatRoute) {
	request, err := protocol.OpenAIResponsesChat(input)
	if err != nil {
		writeOpenAITextChatError(w, err)
		return
	}
	request, images, err := s.prepareOpenAIChat(r.Context(), request)
	if err != nil {
		var invalid *protocol.InputError
		if errors.As(err, &invalid) {
			invalid.Param = responsesInputParam(invalid.Param, input.Instructions)
		}
		writeOpenAITextChatError(w, err)
		return
	}
	suffix := strings.TrimPrefix(newChatID(), "chatcmpl-")
	id, messageID, created := "resp_"+suffix, "msg_"+suffix, time.Now().Unix()
	var text strings.Builder
	response := func(status string, failure error) map[string]any {
		output := []any{}
		itemStatus := "completed"
		if status == "failed" {
			itemStatus = "incomplete"
		}
		if text.Len() > 0 {
			output = append(output, responseTextItem(messageID, itemStatus, text.String(), true))
		}
		var responseError any
		if failure != nil {
			detail := openAIChatErrorObject(failure)
			responseError = map[string]any{"code": detail["code"], "message": detail["message"]}
		}
		return map[string]any{"id": id, "object": "response", "created_at": created, "status": status, "error": responseError, "incomplete_details": nil, "model": input.Model, "output": output, "usage": nil, "instructions": input.Instructions, "store": false, "background": false, "tools": []any{}, "tool_choice": "none", "parallel_tool_calls": false, "metadata": map[string]any{}, "text": map[string]any{"format": map[string]any{"type": "text"}}}
	}
	if !input.Stream {
		err = s.runOpenAIChat(r, request, route, images, func(event provider.OpenAIChatEvent) error { text.WriteString(event.Text); return nil })
		if err != nil {
			writeOpenAITextChatError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, response("completed", nil))
		return
	}
	setOpenAIStreamHeaders(w)
	sequence := 0
	emit := func(kind string, data map[string]any) error {
		data["type"], data["sequence_number"] = kind, sequence
		sequence++
		return writeOpenAIStreamData(w, kind, data)
	}
	if emit("response.created", map[string]any{"response": response("in_progress", nil)}) != nil {
		return
	}
	if emit("response.in_progress", map[string]any{"response": response("in_progress", nil)}) != nil {
		return
	}
	started := false
	err = s.runOpenAIChat(r, request, route, images, func(event provider.OpenAIChatEvent) error {
		if event.Text == "" {
			return nil
		}
		if !started {
			if err := emit("response.output_item.added", map[string]any{"output_index": 0, "item": responseTextItem(messageID, "in_progress", "", false)}); err != nil {
				return err
			}
			if err := emit("response.content_part.added", map[string]any{"output_index": 0, "item_id": messageID, "content_index": 0, "part": responseTextPart("")}); err != nil {
				return err
			}
			started = true
		}
		text.WriteString(event.Text)
		return emit("response.output_text.delta", map[string]any{"output_index": 0, "item_id": messageID, "content_index": 0, "delta": event.Text, "logprobs": []any{}})
	})
	if r.Context().Err() != nil {
		return
	}
	if err != nil {
		_ = emit("response.failed", map[string]any{"response": response("failed", err)})
		return
	}
	if emit("response.output_text.done", map[string]any{"output_index": 0, "item_id": messageID, "content_index": 0, "text": text.String(), "logprobs": []any{}}) != nil {
		return
	}
	if emit("response.content_part.done", map[string]any{"output_index": 0, "item_id": messageID, "content_index": 0, "part": responseTextPart(text.String())}) != nil {
		return
	}
	if emit("response.output_item.done", map[string]any{"output_index": 0, "item": responseTextItem(messageID, "completed", text.String(), true)}) != nil {
		return
	}
	_ = emit("response.completed", map[string]any{"response": response("completed", nil)})
}

func responsesInputParam(param, instructions string) string {
	param = strings.Replace(param, "messages", "input", 1)
	if strings.TrimSpace(instructions) == "" || !strings.HasPrefix(param, "input[") {
		return param
	}
	end := strings.Index(param, "]")
	if end < 0 {
		return param
	}
	index, err := strconv.Atoi(param[len("input["):end])
	if err != nil {
		return param
	}
	if index == 0 {
		return "instructions"
	}
	return fmt.Sprintf("input[%d]%s", index-1, param[end+1:])
}

package converter

import (
	"encoding/json"
	"fmt"
)

// --- Anthropic request types ---

type anthropicRequest struct {
	Model         string            `json:"model"`
	MaxTokens     int               `json:"max_tokens,omitempty"`
	System        json.RawMessage   `json:"system,omitempty"`
	Messages      []json.RawMessage `json:"messages"`
	Stream        *bool             `json:"stream,omitempty"`
	StopSequences []string          `json:"stop_sequences,omitempty"`
	Temperature   *float64          `json:"temperature,omitempty"`
	TopP          *float64          `json:"top_p,omitempty"`
	TopK          *int              `json:"top_k,omitempty"`
	Tools         json.RawMessage   `json:"tools,omitempty"`
	ToolChoice    json.RawMessage   `json:"tool_choice,omitempty"`
}

// --- OpenAI request types ---

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    interface{}      `json:"content,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIToolCall struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	Function openAIToolCallFunction `json:"function"`
}

type openAIToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAITool struct {
	Type     string         `json:"type"`
	Function openAIFunction `json:"function"`
}

type openAIFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type openAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIRequest struct {
	Model         string              `json:"model"`
	MaxTokens     int                 `json:"max_tokens,omitempty"`
	Messages      []openAIMessage     `json:"messages"`
	Stream        *bool               `json:"stream,omitempty"`
	StreamOptions *openAIStreamOptions `json:"stream_options,omitempty"`
	Stop          []string            `json:"stop,omitempty"`
	Temperature   *float64            `json:"temperature,omitempty"`
	TopP          *float64            `json:"top_p,omitempty"`
	Tools         []openAITool        `json:"tools,omitempty"`
	ToolChoice    interface{}         `json:"tool_choice,omitempty"`
}

func convertAnthropic(body []byte) ([]byte, error) {
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("parsing anthropic request: %w", err)
	}

	var messages []openAIMessage

	// Convert system field to a system message.
	if len(req.System) > 0 {
		sysText, err := extractText(req.System)
		if err != nil {
			return nil, fmt.Errorf("parsing system field: %w", err)
		}
		if sysText != "" {
			messages = append(messages, openAIMessage{Role: "system", Content: sysText})
		}
	}

	// Convert messages.
	for i, raw := range req.Messages {
		converted, err := convertAnthropicMessage(raw, i)
		if err != nil {
			return nil, err
		}
		messages = append(messages, converted...)
	}

	out := openAIRequest{
		Model:       req.Model,
		MaxTokens:   req.MaxTokens,
		Messages:    messages,
		Stream:      req.Stream,
		Stop:        req.StopSequences,
		Temperature: req.Temperature,
		TopP:        req.TopP,
	}

	// When streaming, ask the backend to include token usage in the final chunk
	// so the cost tracker can record it.
	if req.Stream != nil && *req.Stream {
		out.StreamOptions = &openAIStreamOptions{IncludeUsage: true}
	}

	// Convert tools.
	if len(req.Tools) > 0 {
		tools, err := convertAnthropicTools(req.Tools)
		if err != nil {
			return nil, fmt.Errorf("converting tools: %w", err)
		}
		out.Tools = tools
	}

	// Convert tool_choice.
	if len(req.ToolChoice) > 0 {
		tc, err := convertAnthropicToolChoice(req.ToolChoice)
		if err != nil {
			return nil, fmt.Errorf("converting tool_choice: %w", err)
		}
		out.ToolChoice = tc
	}

	return json.Marshal(out)
}

// convertAnthropicMessage converts a single Anthropic message to one or more OpenAI messages.
// A single Anthropic message with mixed content (text + tool_use, or multiple tool_results)
// may produce multiple OpenAI messages.
func convertAnthropicMessage(raw json.RawMessage, idx int) ([]openAIMessage, error) {
	var msg struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, fmt.Errorf("parsing message %d: %w", idx, err)
	}

	// Try content as plain string first.
	var s string
	if json.Unmarshal(msg.Content, &s) == nil {
		return []openAIMessage{{Role: msg.Role, Content: s}}, nil
	}

	// Parse as array of content blocks.
	var blocks []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return nil, fmt.Errorf("parsing message %d content: %w", idx, err)
	}

	var textParts string
	var toolCalls []openAIToolCall
	var result []openAIMessage

	for _, b := range blocks {
		switch b.Type {
		case "text":
			if textParts != "" {
				textParts += "\n"
			}
			textParts += b.Text

		case "thinking":
			// Extended thinking blocks — drop for OpenAI (not supported).
			continue

		case "tool_use":
			args, err := json.Marshal(b.Input)
			if err != nil {
				args = b.Input
			}
			toolCalls = append(toolCalls, openAIToolCall{
				ID:   b.ID,
				Type: "function",
				Function: openAIToolCallFunction{
					Name:      b.Name,
					Arguments: string(args),
				},
			})

		case "tool_result":
			// Each tool_result becomes its own OpenAI "tool" role message.
			content := extractToolResultContent(b.Content)
			result = append(result, openAIMessage{
				Role:       "tool",
				Content:    content,
				ToolCallID: b.ToolUseID,
			})
		}
	}

	// Emit the assistant message with text and/or tool_calls.
	if msg.Role == "assistant" && (textParts != "" || len(toolCalls) > 0) {
		m := openAIMessage{Role: "assistant"}
		if textParts != "" {
			m.Content = textParts
		}
		if len(toolCalls) > 0 {
			m.ToolCalls = toolCalls
		}
		// Prepend assistant message before any tool results.
		result = append([]openAIMessage{m}, result...)
	} else if msg.Role == "user" && textParts != "" && len(result) == 0 {
		// Plain user text (no tool_results in this message).
		result = append(result, openAIMessage{Role: "user", Content: textParts})
	} else if msg.Role == "user" && textParts != "" && len(result) > 0 {
		// User message that has both text and tool_results — prepend the text.
		result = append([]openAIMessage{{Role: "user", Content: textParts}}, result...)
	}

	if len(result) == 0 {
		// Fallback: empty content.
		result = append(result, openAIMessage{Role: msg.Role, Content: ""})
	}

	return result, nil
}

// extractToolResultContent handles the tool_result content field which can be
// a string, an array of content blocks, or absent.
func extractToolResultContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	// Try as string.
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}

	// Try as array of content blocks.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var result string
		for _, b := range blocks {
			if b.Type == "text" {
				if result != "" {
					result += "\n"
				}
				result += b.Text
			}
		}
		return result
	}

	return string(raw)
}

// convertAnthropicTools converts Anthropic tool definitions to OpenAI format.
func convertAnthropicTools(raw json.RawMessage) ([]openAITool, error) {
	var tools []struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		InputSchema json.RawMessage `json:"input_schema,omitempty"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, err
	}

	var result []openAITool
	for _, t := range tools {
		result = append(result, openAITool{
			Type: "function",
			Function: openAIFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}
	return result, nil
}

// convertAnthropicToolChoice converts Anthropic tool_choice to OpenAI format.
func convertAnthropicToolChoice(raw json.RawMessage) (interface{}, error) {
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name,omitempty"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return nil, err
	}

	switch tc.Type {
	case "auto":
		return "auto", nil
	case "any":
		return "required", nil
	case "tool":
		return map[string]interface{}{
			"type":     "function",
			"function": map[string]string{"name": tc.Name},
		}, nil
	case "none":
		return "none", nil
	default:
		return "auto", nil
	}
}

// extractText handles both a plain string and an array of Anthropic content blocks.
func extractText(raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}

	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", fmt.Errorf("content is neither string nor content blocks: %s", raw)
	}

	var result string
	for _, b := range blocks {
		if b.Type == "text" {
			if result != "" {
				result += "\n"
			}
			result += b.Text
		}
	}
	return result, nil
}

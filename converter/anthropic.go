package converter

import (
	"encoding/json"
	"fmt"
)

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
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIRequest struct {
	Model       string          `json:"model"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Messages    []openAIMessage `json:"messages"`
	Stream      *bool           `json:"stream,omitempty"`
	Stop        []string        `json:"stop,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
}

func convertAnthropic(body []byte) ([]byte, error) {
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("parsing anthropic request: %w", err)
	}

	var messages []openAIMessage

	// Convert system field to a system message.
	// Anthropic system can be a string or an array of content blocks.
	if len(req.System) > 0 {
		sysText, err := extractText(req.System)
		if err != nil {
			return nil, fmt.Errorf("parsing system field: %w", err)
		}
		if sysText != "" {
			messages = append(messages, openAIMessage{Role: "system", Content: sysText})
		}
	}

	// Convert messages — Anthropic content can be a string or content blocks.
	for i, raw := range req.Messages {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			return nil, fmt.Errorf("parsing message %d: %w", i, err)
		}

		text, err := extractText(msg.Content)
		if err != nil {
			return nil, fmt.Errorf("parsing message %d content: %w", i, err)
		}
		messages = append(messages, openAIMessage{Role: msg.Role, Content: text})
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

	return json.Marshal(out)
}

// extractText handles both a plain string and an array of Anthropic content blocks.
// Content blocks: [{"type":"text","text":"hello"}, ...]
func extractText(raw json.RawMessage) (string, error) {
	// Try as plain string first.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}

	// Try as array of content blocks.
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

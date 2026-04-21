package converter

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"strings"
)

// --- OpenAI response types ---

type openAIResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
	Usage   *openAIUsage   `json:"usage,omitempty"`
}

type openAIChoice struct {
	Index        int            `json:"index"`
	Message      *openAIRespMsg `json:"message,omitempty"`
	Delta        *openAIRespMsg `json:"delta,omitempty"`
	FinishReason *string        `json:"finish_reason,omitempty"`
}

type openAIRespMsg struct {
	Role                   string                      `json:"role,omitempty"`
	Content                string                      `json:"content,omitempty"`
	ToolCalls              []openAIRespToolCall         `json:"tool_calls,omitempty"`
	Images                 []openAIRespImage            `json:"images,omitempty"`
	ProviderSpecificFields *openAIProviderSpecific      `json:"provider_specific_fields,omitempty"`
}

type openAIProviderSpecific struct {
	Content string `json:"content,omitempty"`
}

// EffectiveContent returns the text content from either the standard content
// field or from provider_specific_fields.content (used by some thinking models).
func (m *openAIRespMsg) EffectiveContent() string {
	if m.Content != "" {
		return m.Content
	}
	if m.ProviderSpecificFields != nil && m.ProviderSpecificFields.Content != "" {
		return m.ProviderSpecificFields.Content
	}
	return ""
}

type openAIRespImage struct {
	ImageURL openAIRespImageURL `json:"image_url"`
}

type openAIRespImageURL struct {
	URL string `json:"url"`
}

type openAIRespToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// --- Anthropic response types ---

type anthropicResponse struct {
	ID         string             `json:"id"`
	Type       string             `json:"type"`
	Role       string             `json:"role"`
	Model      string             `json:"model"`
	Content    []anthropicContent `json:"content"`
	StopReason string             `json:"stop_reason"`
	Usage      anthropicUsage     `json:"usage"`
}

type anthropicContent struct {
	Type   string               `json:"type"`
	Text   string               `json:"text,omitempty"`
	ID     string               `json:"id,omitempty"`
	Name   string               `json:"name,omitempty"`
	Input  json.RawMessage      `json:"input,omitempty"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// mapFinishReason converts OpenAI finish_reason to Anthropic stop_reason.
func mapFinishReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "end_turn"
	default:
		return "end_turn"
	}
}

// ConvertResponseToAnthropic converts an OpenAI chat completion response to Anthropic format.
func ConvertResponseToAnthropic(body []byte) ([]byte, error) {
	var resp openAIResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing openai response: %w", err)
	}

	var content []anthropicContent
	stopReason := "end_turn"

	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		if choice.Message != nil {
			if txt := choice.Message.EffectiveContent(); txt != "" {
				content = append(content, anthropicContent{
					Type: "text",
					Text: txt,
				})
			}
			for _, tc := range choice.Message.ToolCalls {
				var input json.RawMessage
				if tc.Function.Arguments != "" {
					input = json.RawMessage(tc.Function.Arguments)
				} else {
					input = json.RawMessage("{}")
				}
				content = append(content, anthropicContent{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: input,
				})
			}
			// Convert images to text placeholders (the Anthropic "image" content
			// block type is not recognised by older SDK versions used by some clients).
			for range choice.Message.Images {
				content = append(content, anthropicContent{
					Type: "text",
					Text: "[image omitted]",
				})
			}
		}
		if choice.FinishReason != nil {
			stopReason = mapFinishReason(*choice.FinishReason)
		}
	}

	if content == nil {
		content = []anthropicContent{}
	}

	var u anthropicUsage
	if resp.Usage != nil {
		u.InputTokens = resp.Usage.PromptTokens
		u.OutputTokens = resp.Usage.CompletionTokens
	}

	id := resp.ID
	if strings.HasPrefix(id, "chatcmpl-") {
		id = "msg_" + id[len("chatcmpl-"):]
	}

	out := anthropicResponse{
		ID:         id,
		Type:       "message",
		Role:       "assistant",
		Model:      resp.Model,
		Content:    content,
		StopReason: stopReason,
		Usage:      u,
	}

	return json.Marshal(out)
}

// --- Streaming converter ---

// StreamingAnthropicConverter wraps an OpenAI SSE stream body and converts
// it to Anthropic SSE format on-the-fly, without buffering the entire response.
type StreamingAnthropicConverter struct {
	inner   io.ReadCloser
	scanner *bufio.Scanner
	buf     bytes.Buffer // unconsumed converted output

	// State tracking
	started         bool // emitted message_start?
	contentStarted  bool // emitted content_block_start for text?
	blockIndex      int  // current content block index
	model           string
	id              string
	inputTokens     int
	outputTokens    int
	finishReason    string

	// Tool call state: OpenAI streams tool_calls incrementally.
	// We track which tool call indices we've started blocks for.
	toolCallStarted map[int]bool
	done            bool
}

// NewStreamingAnthropicConverter wraps an OpenAI SSE response body.
func NewStreamingAnthropicConverter(body io.ReadCloser) io.ReadCloser {
	s := &StreamingAnthropicConverter{
		inner:           body,
		scanner:         bufio.NewScanner(body),
		toolCallStarted: make(map[int]bool),
	}
	// Allow large SSE lines (image data URLs can exceed 1 MB).
	s.scanner.Buffer(make([]byte, 0, 256*1024), 8*1024*1024)
	return s
}

func (s *StreamingAnthropicConverter) Read(p []byte) (int, error) {
	for s.buf.Len() == 0 {
		if s.done {
			return 0, io.EOF
		}
		if !s.scanner.Scan() {
			if err := s.scanner.Err(); err != nil {
				return 0, err
			}
			// End of input — emit closing events if needed.
			s.emitClose()
			s.done = true
			if s.buf.Len() == 0 {
				return 0, io.EOF
			}
			break
		}
		line := s.scanner.Text()
		s.processLine(line)
	}
	return s.buf.Read(p)
}

func (s *StreamingAnthropicConverter) Close() error {
	return s.inner.Close()
}

func (s *StreamingAnthropicConverter) processLine(line string) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data: ") {
		return
	}
	payload := line[len("data: "):]
	if payload == "[DONE]" {
		s.emitClose()
		s.done = true
		return
	}

	var chunk openAIResponse
	if json.Unmarshal([]byte(payload), &chunk) != nil {
		return
	}

	if chunk.Model != "" {
		s.model = chunk.Model
	}
	if chunk.ID != "" {
		s.id = chunk.ID
		if strings.HasPrefix(s.id, "chatcmpl-") {
			s.id = "msg_" + s.id[len("chatcmpl-"):]
		}
	}
	if chunk.Usage != nil {
		s.inputTokens = chunk.Usage.PromptTokens
		s.outputTokens = chunk.Usage.CompletionTokens
	}

	// Emit message_start on first chunk.
	if !s.started {
		s.started = true
		s.writeSSE("message_start", map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id":      s.id,
				"type":    "message",
				"role":    "assistant",
				"model":   s.model,
				"content": []interface{}{},
				"usage":   map[string]int{"input_tokens": s.inputTokens, "output_tokens": 0},
			},
		})
	}

	if len(chunk.Choices) == 0 {
		return
	}
	choice := chunk.Choices[0]
	delta := choice.Delta
	if delta == nil {
		// Some chunks only carry usage or finish_reason.
		if choice.FinishReason != nil {
			s.finishReason = *choice.FinishReason
		}
		return
	}

	// Text content delta.
	if txt := delta.EffectiveContent(); txt != "" {
		if !s.contentStarted {
			s.contentStarted = true
			s.writeSSE("content_block_start", map[string]interface{}{
				"type":          "content_block_start",
				"index":         s.blockIndex,
				"content_block": map[string]string{"type": "text", "text": ""},
			})
		}
		s.writeSSE("content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": s.blockIndex,
			"delta": map[string]string{"type": "text_delta", "text": txt},
		})
	}

	// Image deltas — emit as text placeholders instead of "image" content blocks
	// because older Anthropic SDK versions (used by some clients) don't recognise
	// the "image" block type and throw a Zod validation error.
	for range delta.Images {
		// Close text block if open.
		if s.contentStarted {
			s.writeSSE("content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": s.blockIndex,
			})
			s.blockIndex++
			s.contentStarted = false
		}
		s.writeSSE("content_block_start", map[string]interface{}{
			"type":          "content_block_start",
			"index":         s.blockIndex,
			"content_block": map[string]string{"type": "text", "text": ""},
		})
		s.writeSSE("content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": s.blockIndex,
			"delta": map[string]string{"type": "text_delta", "text": "[image omitted]"},
		})
		s.writeSSE("content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": s.blockIndex,
		})
		s.blockIndex++
	}

	// Tool call deltas.
	for _, tc := range delta.ToolCalls {
		s.processToolCallDelta(tc)
	}

	if choice.FinishReason != nil {
		s.finishReason = *choice.FinishReason
	}
}

// openAIStreamToolCall represents a tool call delta in a streaming chunk.
type openAIStreamToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

func (s *StreamingAnthropicConverter) processToolCallDelta(tc openAIRespToolCall) {
	// OpenAI streaming: tool_calls in delta include an index field.
	// We need to parse the raw delta for the index. Since we typed it
	// as openAIRespToolCall (no index), we use tc.ID presence to detect new vs continued.

	// For simplicity: if we see an ID, it's a new tool call start.
	if tc.ID != "" && !s.toolCallStarted[len(s.toolCallStarted)] {
		// Close text block if open.
		if s.contentStarted {
			s.writeSSE("content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": s.blockIndex,
			})
			s.blockIndex++
			s.contentStarted = false
		}
		// Close any previous tool block if there's already one open.
		idx := len(s.toolCallStarted)
		s.toolCallStarted[idx] = true

		s.writeSSE("content_block_start", map[string]interface{}{
			"type":  "content_block_start",
			"index": s.blockIndex,
			"content_block": map[string]interface{}{
				"type":  "tool_use",
				"id":    tc.ID,
				"name":  tc.Function.Name,
				"input": map[string]interface{}{},
			},
		})
	}

	// Emit argument fragments as input_json_delta.
	if tc.Function.Arguments != "" {
		s.writeSSE("content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": s.blockIndex,
			"delta": map[string]string{
				"type":         "input_json_delta",
				"partial_json": tc.Function.Arguments,
			},
		})
	}
}

func (s *StreamingAnthropicConverter) emitClose() {
	if !s.started {
		return
	}

	// Close any open content block.
	if s.contentStarted || len(s.toolCallStarted) > 0 {
		s.writeSSE("content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": s.blockIndex,
		})
	}

	stopReason := "end_turn"
	if s.finishReason != "" {
		stopReason = mapFinishReason(s.finishReason)
	}

	s.writeSSE("message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]string{"stop_reason": stopReason},
		"usage": map[string]int{"output_tokens": s.outputTokens},
	})

	s.writeSSE("message_stop", map[string]interface{}{
		"type": "message_stop",
	})
}

func (s *StreamingAnthropicConverter) writeSSE(event string, data interface{}) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(&s.buf, "event: %s\ndata: %s\n\n", event, string(b))
}

// ConvertStreamToAnthropic converts a buffered OpenAI SSE stream to Anthropic SSE format.
// Kept for non-streaming proxy use (e.g. tests). For real streaming, use StreamingAnthropicConverter.
func ConvertStreamToAnthropic(body []byte) []byte {
	r := NewStreamingAnthropicConverter(ioutil.NopCloser(bytes.NewReader(body)))
	out, _ := ioutil.ReadAll(r)
	return out
}

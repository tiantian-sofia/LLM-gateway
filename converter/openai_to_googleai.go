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

// --- Google AI (Gemini) response types ---

type googleAIResponse struct {
	Candidates    []googleAICandidate  `json:"candidates"`
	UsageMetadata *googleAIUsageMeta   `json:"usageMetadata,omitempty"`
}

type googleAICandidate struct {
	Content      *googleAIContent `json:"content,omitempty"`
	FinishReason string           `json:"finishReason,omitempty"`
	Index        int              `json:"index"`
}

type googleAIUsageMeta struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

// mapFinishReasonToGemini converts OpenAI finish_reason to Gemini finishReason.
func mapFinishReasonToGemini(reason string) string {
	switch reason {
	case "stop":
		return "STOP"
	case "length":
		return "MAX_TOKENS"
	case "tool_calls":
		return "STOP"
	case "content_filter":
		return "SAFETY"
	default:
		return "STOP"
	}
}

// ConvertResponseToGoogleAI converts an OpenAI chat completion response to
// Google AI (Gemini) GenerateContentResponse format.
func ConvertResponseToGoogleAI(body []byte) ([]byte, error) {
	var resp openAIResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing openai response: %w", err)
	}

	var parts []googleAIPart
	finishReason := "STOP"

	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		if choice.Message != nil {
			if txt := choice.Message.EffectiveContent(); txt != "" {
				parts = append(parts, googleAIPart{Text: txt})
			}
			for _, img := range choice.Message.Images {
				mimeType, data := parseDataURL(img.ImageURL.URL)
				if data != "" {
					parts = append(parts, googleAIPart{
						InlineData: &googleAIInlineData{
							MimeType: mimeType,
							Data:     data,
						},
					})
				}
			}
		}
		if choice.FinishReason != nil {
			finishReason = mapFinishReasonToGemini(*choice.FinishReason)
		}
	}

	if parts == nil {
		parts = []googleAIPart{{Text: ""}}
	}

	candidate := googleAICandidate{
		Content: &googleAIContent{
			Parts: parts,
			Role:  "model",
		},
		FinishReason: finishReason,
		Index:        0,
	}

	out := googleAIResponse{
		Candidates: []googleAICandidate{candidate},
	}

	if resp.Usage != nil {
		out.UsageMetadata = &googleAIUsageMeta{
			PromptTokenCount:     resp.Usage.PromptTokens,
			CandidatesTokenCount: resp.Usage.CompletionTokens,
			TotalTokenCount:      resp.Usage.TotalTokens,
		}
	}

	return json.Marshal(out)
}

// parseDataURL extracts the MIME type and base64 data from a data URL.
// e.g. "data:image/jpeg;base64,/9j/4AAQ..." → ("image/jpeg", "/9j/4AAQ...")
func parseDataURL(url string) (mimeType, data string) {
	const prefix = "data:"
	if !strings.HasPrefix(url, prefix) {
		return "application/octet-stream", url
	}
	rest := url[len(prefix):]
	idx := strings.Index(rest, ",")
	if idx < 0 {
		return "application/octet-stream", rest
	}
	meta := rest[:idx]
	data = rest[idx+1:]
	// meta is e.g. "image/jpeg;base64"
	parts := strings.SplitN(meta, ";", 2)
	mimeType = parts[0]
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	return mimeType, data
}

// --- Streaming converter ---

// StreamingGeminiConverter wraps an OpenAI SSE stream body and converts
// it to Google AI (Gemini) SSE format on-the-fly, without buffering the
// entire response. Each OpenAI chunk becomes a Gemini GenerateContentResponse.
type StreamingGeminiConverter struct {
	inner   io.ReadCloser
	scanner *bufio.Scanner
	buf     bytes.Buffer // unconsumed converted output

	// Accumulated state
	inputTokens  int
	outputTokens int
	totalTokens  int
	done         bool
}

// NewStreamingGeminiConverter wraps an OpenAI SSE response body.
func NewStreamingGeminiConverter(body io.ReadCloser) io.ReadCloser {
	s := &StreamingGeminiConverter{
		inner:   body,
		scanner: bufio.NewScanner(body),
	}
	// Allow large SSE lines (image data URLs can exceed 1 MB).
	s.scanner.Buffer(make([]byte, 0, 256*1024), 8*1024*1024)
	return s
}

func (s *StreamingGeminiConverter) Read(p []byte) (int, error) {
	for s.buf.Len() == 0 {
		if s.done {
			return 0, io.EOF
		}
		if !s.scanner.Scan() {
			if err := s.scanner.Err(); err != nil {
				return 0, err
			}
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

func (s *StreamingGeminiConverter) Close() error {
	return s.inner.Close()
}

func (s *StreamingGeminiConverter) processLine(line string) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data: ") {
		return
	}
	payload := line[len("data: "):]
	if payload == "[DONE]" {
		s.done = true
		return
	}

	var chunk openAIResponse
	if json.Unmarshal([]byte(payload), &chunk) != nil {
		return
	}

	// Track usage from the final chunk.
	if chunk.Usage != nil {
		s.inputTokens = chunk.Usage.PromptTokens
		s.outputTokens = chunk.Usage.CompletionTokens
		s.totalTokens = chunk.Usage.TotalTokens
	}

	if len(chunk.Choices) == 0 {
		// Usage-only chunk — emit usage as a final Gemini chunk if we have it.
		if chunk.Usage != nil {
			resp := googleAIResponse{
				Candidates: []googleAICandidate{{
					Content: &googleAIContent{
						Parts: []googleAIPart{{Text: ""}},
						Role:  "model",
					},
					Index: 0,
				}},
				UsageMetadata: &googleAIUsageMeta{
					PromptTokenCount:     s.inputTokens,
					CandidatesTokenCount: s.outputTokens,
					TotalTokenCount:      s.totalTokens,
				},
			}
			s.writeSSE(resp)
		}
		return
	}

	choice := chunk.Choices[0]
	delta := choice.Delta
	if delta == nil && choice.FinishReason == nil {
		return
	}

	// Build a Gemini response chunk.
	resp := googleAIResponse{
		Candidates: []googleAICandidate{{
			Index: 0,
		}},
	}
	candidate := &resp.Candidates[0]

	// Text content and images.
	if delta != nil {
		var parts []googleAIPart
		if txt := delta.EffectiveContent(); txt != "" {
			parts = append(parts, googleAIPart{Text: txt})
		}
		for _, img := range delta.Images {
			mimeType, data := parseDataURL(img.ImageURL.URL)
			if data != "" {
				parts = append(parts, googleAIPart{
					InlineData: &googleAIInlineData{
						MimeType: mimeType,
						Data:     data,
					},
				})
			}
		}
		if len(parts) > 0 {
			candidate.Content = &googleAIContent{
				Parts: parts,
				Role:  "model",
			}
		}
	}

	// Finish reason.
	if choice.FinishReason != nil {
		candidate.FinishReason = mapFinishReasonToGemini(*choice.FinishReason)
		// Include usage in the final chunk with finish reason.
		if s.inputTokens > 0 || s.outputTokens > 0 {
			resp.UsageMetadata = &googleAIUsageMeta{
				PromptTokenCount:     s.inputTokens,
				CandidatesTokenCount: s.outputTokens,
				TotalTokenCount:      s.totalTokens,
			}
		}
	}

	// Only emit if we have content or a finish reason.
	if candidate.Content != nil || candidate.FinishReason != "" {
		s.writeSSE(resp)
	}
}

func (s *StreamingGeminiConverter) writeSSE(data interface{}) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(&s.buf, "data: %s\n\n", string(b))
}

// ConvertStreamToGoogleAI converts a buffered OpenAI SSE stream to Google AI SSE format.
// Kept for non-streaming proxy use (e.g. tests). For real streaming, use StreamingGeminiConverter.
func ConvertStreamToGoogleAI(body []byte) []byte {
	r := NewStreamingGeminiConverter(ioutil.NopCloser(bytes.NewReader(body)))
	out, _ := ioutil.ReadAll(r)
	return out
}

package converter

import (
	"encoding/json"
	"fmt"
)

// Google AI (Gemini) request format:
// POST /v1/models/{model}:generateContent
// POST /v1/models/{model}:streamGenerateContent
// {
//   "contents": [{"role":"user","parts":[{"text":"hello"}]}],
//   "systemInstruction": {"parts":[{"text":"You are helpful"}]},
//   "generationConfig": {"maxOutputTokens":1024,"temperature":0.7,"topP":0.9,"stopSequences":["END"]}
// }

type googleAIRequest struct {
	Contents          []googleAIContent  `json:"contents"`
	SystemInstruction *googleAIContent   `json:"systemInstruction,omitempty"`
	GenerationConfig  *googleAIGenConfig `json:"generationConfig,omitempty"`
}

type googleAIContent struct {
	Role  string         `json:"role,omitempty"`
	Parts []googleAIPart `json:"parts"`
}

type googleAIPart struct {
	Text string `json:"text"`
}

type googleAIGenConfig struct {
	MaxOutputTokens *int      `json:"maxOutputTokens,omitempty"`
	Temperature     *float64  `json:"temperature,omitempty"`
	TopP            *float64  `json:"topP,omitempty"`
	TopK            *int      `json:"topK,omitempty"`
	StopSequences   []string  `json:"stopSequences,omitempty"`
}

func convertGoogleAI(body []byte, model string, stream bool) ([]byte, error) {
	var req googleAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("parsing google ai request: %w", err)
	}

	var messages []openAIMessage

	// Convert systemInstruction to system message.
	if req.SystemInstruction != nil {
		text := partsToText(req.SystemInstruction.Parts)
		if text != "" {
			messages = append(messages, openAIMessage{Role: "system", Content: text})
		}
	}

	// Convert contents to messages.
	for i, c := range req.Contents {
		role := c.Role
		// Google AI uses "model" for assistant role.
		if role == "model" {
			role = "assistant"
		}
		if role == "" {
			role = "user"
		}
		text := partsToText(c.Parts)
		if text == "" {
			return nil, fmt.Errorf("content %d has no text parts", i)
		}
		messages = append(messages, openAIMessage{Role: role, Content: text})
	}

	out := openAIRequest{
		Model:    model,
		Messages: messages,
	}

	if stream {
		b := true
		out.Stream = &b
	}

	if req.GenerationConfig != nil {
		gc := req.GenerationConfig
		if gc.MaxOutputTokens != nil {
			out.MaxTokens = *gc.MaxOutputTokens
		}
		out.Temperature = gc.Temperature
		out.TopP = gc.TopP
		out.Stop = gc.StopSequences
	}

	return json.Marshal(out)
}

func partsToText(parts []googleAIPart) string {
	var result string
	for _, p := range parts {
		if p.Text != "" {
			if result != "" {
				result += "\n"
			}
			result += p.Text
		}
	}
	return result
}

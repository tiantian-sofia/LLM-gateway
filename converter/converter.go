package converter

import (
	"encoding/json"
	"strings"
)

const OpenAIPath = "/v1/chat/completions"

// ConvertResult holds the output of ConvertToOpenAI.
type ConvertResult struct {
	Body  []byte // converted request body
	Path  string // target path (e.g. /v1/chat/completions)
	Model string // model name extracted from the request
}

// ConvertToOpenAI detects the request format, extracts the model name,
// and converts the body to OpenAI chat completions format.
func ConvertToOpenAI(body []byte, path string) (*ConvertResult, error) {
	body = sanitize(body)

	// Anthropic format: /v1/messages
	if strings.HasPrefix(path, "/v1/messages") {
		model := extractModel(body)
		converted, err := convertAnthropic(body)
		if err != nil {
			return nil, err
		}
		return &ConvertResult{Body: converted, Path: OpenAIPath, Model: model}, nil
	}

	// Google AI format: /v1/models/{model}:generateContent, /v1beta/models/{model}:generateContent, etc.
	if model, stream, ok := parseGoogleAIPath(path); ok {
		converted, err := convertGoogleAI(body, model, stream)
		if err != nil {
			return nil, err
		}
		return &ConvertResult{Body: converted, Path: OpenAIPath, Model: model}, nil
	}

	// Already OpenAI format — pass through, but inject stream_options if streaming.
	model := extractModel(body)
	body = injectStreamOptions(body)
	return &ConvertResult{Body: body, Path: path, Model: model}, nil
}

// injectStreamOptions adds stream_options.include_usage to an OpenAI-format
// request body when streaming is enabled, so the backend returns token usage.
func injectStreamOptions(body []byte) []byte {
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return body
	}
	streamRaw, ok := raw["stream"]
	if !ok {
		return body
	}
	var stream bool
	if json.Unmarshal(streamRaw, &stream) != nil || !stream {
		return body
	}
	// Don't overwrite if already present.
	if _, exists := raw["stream_options"]; exists {
		return body
	}
	raw["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	out, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return out
}

// extractModel pulls the "model" field from a JSON request body.
func extractModel(body []byte) string {
	var m struct {
		Model string `json:"model"`
	}
	json.Unmarshal(body, &m)
	return m.Model
}

// sanitize recursively removes keys whose value is the string "[undefined]"
// from JSON objects and arrays.
func sanitize(body []byte) []byte {
	out, changed := sanitizeValue(body)
	if !changed {
		return body
	}
	return out
}

func sanitizeValue(raw json.RawMessage) (json.RawMessage, bool) {
	// Try as object.
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil && obj != nil {
		changed := false
		for k, v := range obj {
			var s string
			if json.Unmarshal(v, &s) == nil && s == "[undefined]" {
				delete(obj, k)
				changed = true
				continue
			}
			if cleaned, c := sanitizeValue(v); c {
				obj[k] = cleaned
				changed = true
			}
		}
		if changed {
			out, _ := json.Marshal(obj)
			return out, true
		}
		return raw, false
	}

	// Try as array.
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil && arr != nil {
		changed := false
		for i, v := range arr {
			if cleaned, c := sanitizeValue(v); c {
				arr[i] = cleaned
				changed = true
			}
		}
		if changed {
			out, _ := json.Marshal(arr)
			return out, true
		}
		return raw, false
	}

	return raw, false
}

// IsGoogleAIPath returns true if the path matches a Google AI (Gemini) endpoint.
func IsGoogleAIPath(path string) bool {
	_, _, ok := parseGoogleAIPath(path)
	return ok
}

// parseGoogleAIPath matches paths like:
//
//	/v1/models/{model}:generateContent
//	/v1/models/{model}:streamGenerateContent
//	/v1beta/models/{model}:generateContent
//	/v1beta/models/{model}:streamGenerateContent
//
// Returns the model name, whether it's streaming, and whether the path matched.
func parseGoogleAIPath(path string) (model string, stream bool, ok bool) {
	var rest string
	switch {
	case strings.HasPrefix(path, "/v1/models/"):
		rest = path[len("/v1/models/"):]
	case strings.HasPrefix(path, "/v1beta/models/"):
		rest = path[len("/v1beta/models/"):]
	default:
		return "", false, false
	}

	if i := strings.Index(rest, ":generateContent"); i > 0 {
		return rest[:i], false, true
	}
	if i := strings.Index(rest, ":streamGenerateContent"); i > 0 {
		return rest[:i], true, true
	}
	return "", false, false
}

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

	// Google AI format: /v1/models/{model}:generateContent or :streamGenerateContent
	if model, stream, ok := parseGoogleAIPath(path); ok {
		converted, err := convertGoogleAI(body, model, stream)
		if err != nil {
			return nil, err
		}
		return &ConvertResult{Body: converted, Path: OpenAIPath, Model: model}, nil
	}

	// Already OpenAI format — pass through.
	model := extractModel(body)
	return &ConvertResult{Body: body, Path: path, Model: model}, nil
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

// parseGoogleAIPath matches paths like:
//
//	/v1/models/{model}:generateContent
//	/v1/models/{model}:streamGenerateContent
//
// Returns the model name, whether it's streaming, and whether the path matched.
func parseGoogleAIPath(path string) (model string, stream bool, ok bool) {
	const prefix = "/v1/models/"
	if !strings.HasPrefix(path, prefix) {
		return "", false, false
	}
	rest := path[len(prefix):]

	if i := strings.Index(rest, ":generateContent"); i > 0 {
		return rest[:i], false, true
	}
	if i := strings.Index(rest, ":streamGenerateContent"); i > 0 {
		return rest[:i], true, true
	}
	return "", false, false
}

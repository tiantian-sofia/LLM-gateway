package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"strings"

	"github.com/tiantian-sofia/LLM-gateway/config"
)

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// costTracker wraps a response body, scans for usage data as bytes pass through,
// and logs the token cost when the body is closed.
type costTracker struct {
	inner  io.ReadCloser
	model  string
	entry  config.ModelEntry
	buf    bytes.Buffer
	logged bool
}

func newCostTracker(body io.ReadCloser, model string, entry config.ModelEntry) io.ReadCloser {
	if entry.InputCostPerToken == 0 && entry.OutputCostPerToken == 0 {
		return body // no pricing configured, skip tracking
	}
	return &costTracker{inner: body, model: model, entry: entry}
}

func (ct *costTracker) Read(p []byte) (int, error) {
	n, err := ct.inner.Read(p)
	if n > 0 {
		ct.buf.Write(p[:n])
	}
	if err == io.EOF && !ct.logged {
		ct.logCost()
		ct.logged = true
	}
	return n, err
}

func (ct *costTracker) Close() error {
	if !ct.logged {
		ct.logCost()
		ct.logged = true
	}
	return ct.inner.Close()
}

func (ct *costTracker) logCost() {
	data := ct.buf.String()

	var u usage

	// Try streaming format: look for the last SSE chunk with usage data.
	if strings.Contains(data, "data: ") {
		lines := strings.Split(data, "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			line := strings.TrimSpace(lines[i])
			if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
				continue
			}
			payload := line[len("data: "):]
			var chunk struct {
				Usage *usage `json:"usage"`
			}
			if json.Unmarshal([]byte(payload), &chunk) == nil && chunk.Usage != nil {
				u = *chunk.Usage
				break
			}
		}
	} else {
		// Non-streaming: parse the full response body.
		var resp struct {
			Usage *usage `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &resp) == nil && resp.Usage != nil {
			u = *resp.Usage
		}
	}

	if u.TotalTokens == 0 && u.PromptTokens == 0 && u.CompletionTokens == 0 {
		return
	}

	inputCost := float64(u.PromptTokens) * ct.entry.InputCostPerToken
	outputCost := float64(u.CompletionTokens) * ct.entry.OutputCostPerToken
	totalCost := inputCost + outputCost

	log.Printf("[cost] model=%s input_tokens=%d output_tokens=%d total_tokens=%d input_cost=$%.6f output_cost=$%.6f total_cost=$%.6f",
		ct.model, u.PromptTokens, u.CompletionTokens, u.TotalTokens, inputCost, outputCost, totalCost)
}

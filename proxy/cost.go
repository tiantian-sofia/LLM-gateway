package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/tiantian-sofia/LLM-gateway/config"
)

// CostRecord stores the cost data for a single request.
type CostRecord struct {
	Time             time.Time
	Model            string
	SourceIP         string
	UserAgent        string
	InputTokens      int
	OutputTokens     int
	TotalTokens      int
	InputCost        float64
	OutputCost       float64
	TotalCost        float64
}

// CostStore is the interface for persisting cost records to a database.
// Defined here (alongside CostRecord) to avoid import cycles.
type CostStore interface {
	Insert(rec CostRecord) error
	List() ([]CostRecord, error)
	Close() error
}

var (
	costMu      sync.Mutex
	costRecords []CostRecord

	costStore   CostStore
	persistCh   chan CostRecord
)

// SetCostStore configures a persistent store for cost records.
// It loads historical records from the store into the in-memory slice
// and starts a background goroutine to persist new records asynchronously.
func SetCostStore(store CostStore) {
	costMu.Lock()
	defer costMu.Unlock()

	costStore = store
	persistCh = make(chan CostRecord, 256)

	// Load historical records from the database.
	records, err := store.List()
	if err != nil {
		log.Printf("[cost] warning: failed to load historical records from database: %v", err)
	} else if len(records) > 0 {
		costRecords = append(records, costRecords...)
		log.Printf("[cost] loaded %d historical records from database", len(records))
	}

	// Start background writer.
	go persistLoop(persistCh, store)
}

func persistLoop(ch <-chan CostRecord, store CostStore) {
	for rec := range ch {
		if err := store.Insert(rec); err != nil {
			log.Printf("[cost] failed to persist record to database: %v", err)
		}
	}
}

// GetCostRecords returns a copy of all recorded cost entries.
func GetCostRecords() []CostRecord {
	costMu.Lock()
	defer costMu.Unlock()
	out := make([]CostRecord, len(costRecords))
	copy(out, costRecords)
	return out
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// costTracker wraps a response body, scans for usage data as bytes pass through,
// and logs the token cost when the body is closed.
type costTracker struct {
	inner     io.ReadCloser
	model     string
	entry     config.ModelEntry
	sourceIP  string
	userAgent string
	buf       bytes.Buffer
	logged    bool
}

func newCostTracker(body io.ReadCloser, model string, entry config.ModelEntry, sourceIP, userAgent string) io.ReadCloser {
	if entry.InputCostPerToken == 0 && entry.OutputCostPerToken == 0 {
		return body // no pricing configured, skip tracking
	}
	return &costTracker{inner: body, model: model, entry: entry, sourceIP: sourceIP, userAgent: userAgent}
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

	rec := CostRecord{
		Time:         time.Now(),
		Model:        ct.model,
		SourceIP:     ct.sourceIP,
		UserAgent:    ct.userAgent,
		InputTokens:  u.PromptTokens,
		OutputTokens: u.CompletionTokens,
		TotalTokens:  u.TotalTokens,
		InputCost:    inputCost,
		OutputCost:   outputCost,
		TotalCost:    totalCost,
	}
	costMu.Lock()
	costRecords = append(costRecords, rec)
	ch := persistCh // capture under lock
	costMu.Unlock()

	// Send to the persist channel (non-blocking).
	if ch != nil {
		select {
		case ch <- rec:
		default:
			log.Printf("[cost] warning: persist channel full, record not queued for database")
		}
	}
}

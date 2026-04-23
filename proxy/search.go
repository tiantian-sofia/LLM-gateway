package proxy

import (
	"html/template"
	"log"
	"net/http"
	"sort"
	"time"
)

type searchData struct {
	// Filter state (for repopulating form)
	ActiveRange string
	StartDate   string
	EndDate     string
	ModelQuery  string

	// Whether a search was performed
	Searched bool

	// Results
	TotalRequests     int
	TotalInputTokens  int
	TotalOutputTokens int
	TotalTokens       int
	TotalCost         float64
	Models            []modelSummary
	Records           []CostRecord

	// Available models for datalist
	AvailableModels []string
}

// CostSearchHandler serves the search page at /ui/search.
func CostSearchHandler() http.HandlerFunc {
	tmpl := template.Must(template.New("search").Parse(searchHTML))

	return func(w http.ResponseWriter, r *http.Request) {
		models := GetDistinctModels()
		sort.Strings(models)

		data := searchData{
			AvailableModels: models,
		}

		q := r.URL.Query()
		rangeParam := q.Get("range")
		startParam := q.Get("start")
		endParam := q.Get("end")
		modelParam := q.Get("model")

		// Determine if a search was requested.
		if rangeParam == "" && startParam == "" && endParam == "" && modelParam == "" {
			// Initial page load — show form only.
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			tmpl.Execute(w, data)
			return
		}

		data.Searched = true
		data.ModelQuery = modelParam
		data.ActiveRange = rangeParam

		var filter CostFilter
		if modelParam != "" {
			filter.Model = modelParam
		}

		now := time.Now()
		switch rangeParam {
		case "24h":
			t := now.Add(-24 * time.Hour)
			filter.StartTime = &t
		case "7d":
			t := now.Add(-7 * 24 * time.Hour)
			filter.StartTime = &t
		case "30d":
			t := now.Add(-30 * 24 * time.Hour)
			filter.StartTime = &t
		default:
			// Custom date range.
			if startParam != "" {
				if t, err := time.Parse("2006-01-02", startParam); err == nil {
					filter.StartTime = &t
					data.StartDate = startParam
				}
			}
			if endParam != "" {
				if t, err := time.Parse("2006-01-02", endParam); err == nil {
					// End of the selected day.
					endOfDay := t.Add(24*time.Hour - time.Second)
					filter.EndTime = &endOfDay
					data.EndDate = endParam
				}
			}
		}

		// Try DB search first, fall back to in-memory.
		var records []CostRecord
		costMu.Lock()
		store := costStore
		costMu.Unlock()

		if store != nil {
			var err error
			records, err = store.Search(filter)
			if err != nil {
				log.Printf("[search] database search failed, falling back to in-memory: %v", err)
				records = SearchCostRecords(filter)
			}
		} else {
			records = SearchCostRecords(filter)
		}

		// Aggregate results.
		byModel := map[string]*modelSummary{}
		for _, rec := range records {
			data.TotalRequests++
			data.TotalInputTokens += rec.InputTokens
			data.TotalOutputTokens += rec.OutputTokens
			data.TotalTokens += rec.TotalTokens
			data.TotalCost += rec.TotalCost

			ms, ok := byModel[rec.Model]
			if !ok {
				ms = &modelSummary{Model: rec.Model}
				byModel[rec.Model] = ms
			}
			ms.Requests++
			ms.InputTokens += rec.InputTokens
			ms.OutputTokens += rec.OutputTokens
			ms.TotalTokens += rec.TotalTokens
			ms.InputCost += rec.InputCost
			ms.OutputCost += rec.OutputCost
			ms.TotalCost += rec.TotalCost
		}

		for _, ms := range byModel {
			data.Models = append(data.Models, *ms)
		}
		sort.Slice(data.Models, func(i, j int) bool {
			return data.Models[i].TotalCost > data.Models[j].TotalCost
		})

		// Reverse records so newest is first.
		reversed := make([]CostRecord, len(records))
		for i, rec := range records {
			reversed[len(records)-1-i] = rec
		}
		data.Records = reversed

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		tmpl.Execute(w, data)
	}
}

const searchHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>LLM Gateway - Search Costs</title>
<style>
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: #f5f5f5; color: #333; padding: 24px; }
  h1 { font-size: 1.5rem; margin-bottom: 8px; }
  h2 { font-size: 1.1rem; margin: 24px 0 12px; color: #555; }
  .nav { margin-bottom: 20px; font-size: 0.85rem; }
  .nav a { color: #0066cc; text-decoration: none; }
  .nav a:hover { text-decoration: underline; }
  .cards { display: flex; gap: 16px; flex-wrap: wrap; }
  .card { background: #fff; border-radius: 8px; padding: 20px; min-width: 160px; box-shadow: 0 1px 3px rgba(0,0,0,0.1); }
  .card .label { font-size: 0.8rem; color: #888; text-transform: uppercase; letter-spacing: 0.05em; }
  .card .value { font-size: 1.6rem; font-weight: 600; margin-top: 4px; }
  table { width: 100%; border-collapse: collapse; background: #fff; border-radius: 8px; overflow: hidden; box-shadow: 0 1px 3px rgba(0,0,0,0.1); }
  th, td { padding: 10px 14px; text-align: left; border-bottom: 1px solid #eee; font-size: 0.9rem; }
  th { background: #fafafa; font-weight: 600; color: #555; }
  tr:last-child td { border-bottom: none; }
  .mono { font-family: "SF Mono", "Fira Code", monospace; font-size: 0.85rem; }
  .empty { color: #999; padding: 40px; text-align: center; }

  /* Search form styles */
  .search-box { background: #fff; border-radius: 8px; padding: 20px; box-shadow: 0 1px 3px rgba(0,0,0,0.1); margin-bottom: 24px; }
  .quick-filters { display: flex; gap: 8px; margin-bottom: 16px; flex-wrap: wrap; }
  .quick-filters a {
    display: inline-block; padding: 6px 14px; border-radius: 6px; font-size: 0.85rem;
    text-decoration: none; color: #555; background: #f0f0f0; border: 1px solid #ddd;
  }
  .quick-filters a:hover { background: #e0e0e0; }
  .quick-filters a.active { background: #0066cc; color: #fff; border-color: #0066cc; }
  .form-row { display: flex; gap: 12px; align-items: flex-end; flex-wrap: wrap; }
  .form-group { display: flex; flex-direction: column; gap: 4px; }
  .form-group label { font-size: 0.8rem; color: #888; text-transform: uppercase; letter-spacing: 0.05em; }
  .form-group input { padding: 6px 10px; border: 1px solid #ddd; border-radius: 6px; font-size: 0.9rem; }
  .form-group input:focus { outline: none; border-color: #0066cc; }
  .btn { padding: 7px 18px; border-radius: 6px; font-size: 0.9rem; cursor: pointer; border: none; background: #0066cc; color: #fff; }
  .btn:hover { background: #0055aa; }
</style>
</head>
<body>
<h1>Search Cost Records</h1>
<div class="nav"><a href="/ui/costs">&larr; Back to Dashboard</a></div>

<div class="search-box">
  <div class="quick-filters">
    <a href="/ui/search?range=24h{{if .ModelQuery}}&model={{.ModelQuery}}{{end}}"{{if eq .ActiveRange "24h"}} class="active"{{end}}>Last 24h</a>
    <a href="/ui/search?range=7d{{if .ModelQuery}}&model={{.ModelQuery}}{{end}}"{{if eq .ActiveRange "7d"}} class="active"{{end}}>Last 7 days</a>
    <a href="/ui/search?range=30d{{if .ModelQuery}}&model={{.ModelQuery}}{{end}}"{{if eq .ActiveRange "30d"}} class="active"{{end}}>Last 30 days</a>
  </div>
  <form method="GET" action="/ui/search">
    <div class="form-row">
      <div class="form-group">
        <label>Start Date</label>
        <input type="date" name="start" value="{{.StartDate}}">
      </div>
      <div class="form-group">
        <label>End Date</label>
        <input type="date" name="end" value="{{.EndDate}}">
      </div>
      <div class="form-group">
        <label>Model</label>
        <input type="text" name="model" value="{{.ModelQuery}}" list="model-list" placeholder="All models">
        <datalist id="model-list">
          {{range .AvailableModels}}<option value="{{.}}">{{end}}
        </datalist>
      </div>
      <button type="submit" class="btn">Search</button>
    </div>
  </form>
</div>

{{if .Searched}}
<div class="cards">
  <div class="card">
    <div class="label">Matching Requests</div>
    <div class="value">{{.TotalRequests}}</div>
  </div>
  <div class="card">
    <div class="label">Input Tokens</div>
    <div class="value">{{.TotalInputTokens}}</div>
  </div>
  <div class="card">
    <div class="label">Output Tokens</div>
    <div class="value">{{.TotalOutputTokens}}</div>
  </div>
  <div class="card">
    <div class="label">Total Cost</div>
    <div class="value">${{printf "%.4f" .TotalCost}}</div>
  </div>
</div>

{{if .Models}}
<h2>Cost by Model</h2>
<table>
  <thead>
    <tr>
      <th>Model</th>
      <th>Requests</th>
      <th>Input Tokens</th>
      <th>Output Tokens</th>
      <th>Input Cost</th>
      <th>Output Cost</th>
      <th>Total Cost</th>
    </tr>
  </thead>
  <tbody>
    {{range .Models}}
    <tr>
      <td class="mono">{{.Model}}</td>
      <td>{{.Requests}}</td>
      <td>{{.InputTokens}}</td>
      <td>{{.OutputTokens}}</td>
      <td>${{printf "%.6f" .InputCost}}</td>
      <td>${{printf "%.6f" .OutputCost}}</td>
      <td>${{printf "%.6f" .TotalCost}}</td>
    </tr>
    {{end}}
  </tbody>
</table>
{{end}}

<h2>Matching Requests</h2>
{{if .Records}}
<table>
  <thead>
    <tr>
      <th>Time</th>
      <th>Model</th>
      <th>Source IP</th>
      <th>User-Agent</th>
      <th>Input Tokens</th>
      <th>Output Tokens</th>
      <th>Total Tokens</th>
      <th>Cost</th>
    </tr>
  </thead>
  <tbody>
    {{range .Records}}
    <tr>
      <td class="mono">{{.Time.Format "2006-01-02 15:04:05"}}</td>
      <td class="mono">{{.Model}}</td>
      <td class="mono">{{.SourceIP}}</td>
      <td class="mono">{{.UserAgent}}</td>
      <td>{{.InputTokens}}</td>
      <td>{{.OutputTokens}}</td>
      <td>{{.TotalTokens}}</td>
      <td>${{printf "%.6f" .TotalCost}}</td>
    </tr>
    {{end}}
  </tbody>
</table>
{{else}}
<div class="empty">No records match your search criteria.</div>
{{end}}

{{else}}
<div class="empty">Use the filters above to search cost records.</div>
{{end}}

</body>
</html>
`

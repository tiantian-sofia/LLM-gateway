package proxy

import (
	"html/template"
	"net/http"
	"sort"
)

type modelSummary struct {
	Model        string
	Requests     int
	InputTokens  int
	OutputTokens int
	TotalTokens  int
	InputCost    float64
	OutputCost   float64
	TotalCost    float64
}

type dashboardData struct {
	TotalRequests    int
	TotalInputTokens int
	TotalOutputTokens int
	TotalTokens      int
	TotalCost        float64
	Models           []modelSummary
	Records          []CostRecord
}

func CostDashboardHandler() http.HandlerFunc {
	tmpl := template.Must(template.New("dashboard").Parse(dashboardHTML))

	return func(w http.ResponseWriter, r *http.Request) {
		records := GetCostRecords()

		var data dashboardData
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

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<meta http-equiv="refresh" content="30">
<title>LLM Gateway - Token Costs</title>
<style>
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: #f5f5f5; color: #333; padding: 24px; }
  h1 { font-size: 1.5rem; margin-bottom: 24px; }
  h2 { font-size: 1.1rem; margin: 24px 0 12px; color: #555; }
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
  .refresh { font-size: 0.75rem; color: #aaa; margin-top: 16px; }
</style>
</head>
<body>
<h1>LLM Gateway - Token Costs</h1>

<div class="cards">
  <div class="card">
    <div class="label">Total Requests</div>
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

<h2>Cost by Model</h2>
{{if .Models}}
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
{{else}}
<div class="empty">No cost data yet. Send some requests through the gateway.</div>
{{end}}

<h2>Recent Requests</h2>
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
<div class="empty">No cost data yet. Send some requests through the gateway.</div>
{{end}}

<p class="refresh">Auto-refreshes every 30 seconds</p>
</body>
</html>
`

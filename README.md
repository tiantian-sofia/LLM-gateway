# LLM Gateway

A lightweight, high-performance API gateway for Large Language Model providers. Routes requests across multiple backends with automatic load balancing, health-checked failover, cross-format conversion, and real-time cost tracking.

## Features

- **Multi-Provider Support** — Route requests to any combination of OpenAI-compatible, Anthropic (Claude), and Google AI (Gemini) backends
- **Load Balancing** — Round-robin or primary-backup strategies across backend pools
- **Automatic Failover** — On connection errors or 5xx responses, retries the next healthy backend transparently
- **Model Fallback Chains** — Per-model fallback configuration: if all backends for model A's provider are down, automatically try model B on a different provider
- **Format Conversion** — Accept requests in Anthropic or Gemini format and proxy them to OpenAI-compatible backends, converting both request and response (including streaming)
- **Streaming Support** — Full SSE streaming passthrough with real-time format conversion for all three formats
- **Cost Tracking** — Per-token cost calculation with a built-in web dashboard and search page
- **Health Checks** — Active periodic health probes plus passive health marking on failures, with configurable cooldown-based recovery
- **Extra Body Injection** — Inject arbitrary per-model fields into requests (e.g., `thinking` config for extended thinking models)
- **API Key Management** — Server-side API key injection per provider; client keys are replaced automatically
- **PostgreSQL Persistence** — Optional database storage for cost records with indexed search
- **Zero Dependencies** — Pure Go, single binary. Only external dependency is `github.com/lib/pq` for optional PostgreSQL support

## Architecture

```
                    ┌─────────────────────────────────────────────┐
                    │              LLM Gateway (:8080)            │
                    │                                             │
  Anthropic fmt ──▶ │  ┌───────────┐   ┌──────────┐   ┌───────┐ │
  OpenAI fmt    ──▶ │  │  Format   │──▶│   Load   │──▶│Backend│──▶ Provider A
  Gemini fmt    ──▶ │  │ Converter │   │ Balancer │   │ Pool  │──▶ Provider B
                    │  └───────────┘   └──────────┘   └───────┘ │──▶ Provider C
                    │                                             │
                    │  ┌──────────────────────────────────────┐  │
                    │  │  Cost Tracker  │  Health Checker      │  │
                    │  └──────────────────────────────────────┘  │
                    └─────────────────────────────────────────────┘
```

**Request flow:**

1. Incoming request is detected as Anthropic (`/v1/messages`), Gemini (`/v1/models/{model}:generateContent`), or OpenAI (`/v1/chat/completions`) format
2. Request body is converted to OpenAI format internally
3. Model name is looked up to determine the target provider (and fallback chain)
4. Load balancer selects a healthy backend; on failure, tries the next backend, then the fallback model's provider
5. Response is converted back to the caller's original format (Anthropic or Gemini SSE/JSON)
6. Cost tracker extracts `usage` from the response and records token costs

## Supported Providers

| Provider | Auth Header | Request Format | Response Format |
|---|---|---|---|
| **OpenAI / compatible** | `Authorization: Bearer <key>` | OpenAI (native) | OpenAI (native) |
| **Anthropic (Claude)** | `x-api-key: <key>` | Anthropic → OpenAI | OpenAI → Anthropic |
| **Google AI (Gemini)** | `x-goog-api-key: <key>` | Gemini → OpenAI | OpenAI → Gemini |

Provider type is determined by the provider name in config:
- `"claude"` → sets `x-api-key` and `anthropic-version` headers
- `"googleai"` → sets `x-goog-api-key` header
- Anything else → sets `Authorization: Bearer` header (OpenAI-compatible)

## Format Conversion Details

### Request Conversion (→ OpenAI)

| Source Format | Endpoint | Converted To |
|---|---|---|
| Anthropic Messages API | `/v1/messages` | `/v1/chat/completions` |
| Google AI GenerateContent | `/v1/models/{model}:generateContent` | `/v1/chat/completions` |
| Google AI Streaming | `/v1/models/{model}:streamGenerateContent` | `/v1/chat/completions` (stream=true) |
| OpenAI (passthrough) | `/v1/chat/completions` | `/v1/chat/completions` |

**Anthropic → OpenAI conversion handles:**
- System prompt (string or content blocks) → system message
- Content blocks (text, tool_use, tool_result, thinking) → OpenAI message roles
- Tool definitions (`input_schema` → `parameters`)
- Tool choice (`any` → `required`, `tool` → named function)
- Streaming with `stream_options.include_usage` injection

**Gemini → OpenAI conversion handles:**
- `contents` with `parts` → messages
- `systemInstruction` → system message
- `generationConfig` → max_tokens, temperature, top_p, stop sequences
- `"model"` role → `"assistant"` role

### Response Conversion (OpenAI →)

| Target Format | What's Converted |
|---|---|
| **Anthropic** | Message structure, stop_reason mapping, tool_use blocks, usage fields, image placeholders, full SSE event stream (message_start → content_block_start → content_block_delta → content_block_stop → message_delta → message_stop) |
| **Gemini** | candidates/content/parts structure, finishReason mapping, usageMetadata, inline image data, SSE streaming |

## Load Balancing Strategies

### Round-Robin

Distributes requests evenly across all healthy backends in rotation. Unhealthy backends are skipped; backends past their cooldown period are retried.

### Primary-Backup

Always sends to the first healthy backend in the list. Falls back to subsequent backends only when earlier ones are marked unhealthy. Ideal for cost optimization (use a cheaper backend as primary).

## Configuration

Create a `config.json` file (see `config.example.json`):

```json
{
  "listen": ":8080",
  "strategy": "round-robin",
  "providers": {
    "main": {
      "api_key": "sk-your-api-key",
      "backends": [
        {
          "url": "http://backend-1:8000",
          "health_check_path": "/v1/models"
        },
        {
          "url": "http://backend-2:8000",
          "health_check_path": "/v1/models"
        }
      ]
    },
    "fallback": {
      "api_key": "sk-fallback-key",
      "backends": [
        {
          "url": "http://fallback-backend:8000",
          "health_check_path": "/v1/models"
        }
      ]
    }
  },
  "models": {
    "claude-opus-4": {
      "provider": "main",
      "fallback": "claude-opus-4-fallback",
      "input_cost_per_token": 0.000015,
      "output_cost_per_token": 0.000075
    },
    "claude-opus-4-fallback": {
      "provider": "fallback",
      "backend_model": "claude-opus-4",
      "input_cost_per_token": 0.000015,
      "output_cost_per_token": 0.000075
    },
    "gemini-thinking": {
      "provider": "main",
      "extra_body": {
        "thinking": {"type": "enabled", "budget_tokens": 8192},
        "allowed_openai_params": ["thinking"]
      }
    }
  },
  "health_check": {
    "interval_seconds": 3,
    "timeout_seconds": 5,
    "cooldown_seconds": 6
  },
  "request_timeout_seconds": 120,
  "database": {
    "dsn": "postgres://user:password@localhost:5432/llmgateway?sslmode=disable"
  }
}
```

### Configuration Reference

| Field | Type | Default | Description |
|---|---|---|---|
| `listen` | string | `":8080"` | Address and port to listen on |
| `strategy` | string | `"round-robin"` | Load balancing strategy: `"round-robin"` or `"primary-backup"` |
| `providers` | object | *(required)* | Map of provider name → provider config |
| `providers.*.api_key` | string | *(required)* | API key injected into requests to this provider |
| `providers.*.backends` | array | *(required)* | List of backend servers for this provider |
| `providers.*.backends[].url` | string | *(required)* | Backend URL (scheme + host, e.g. `http://host:port`) |
| `providers.*.backends[].health_check_path` | string | `""` | Path to probe for health checks (e.g. `/v1/models`) |
| `models` | object | *(required)* | Map of model name → model config |
| `models.*.provider` | string | *(required)* | Which provider handles this model |
| `models.*.fallback` | string | `""` | Model name to try if this model's provider has no healthy backends |
| `models.*.backend_model` | string | `""` | Rewrite the model name in the request body (for aliases) |
| `models.*.input_cost_per_token` | float | `0` | Cost per input token in USD (enables cost tracking) |
| `models.*.output_cost_per_token` | float | `0` | Cost per output token in USD (enables cost tracking) |
| `models.*.extra_body` | object | `{}` | Arbitrary JSON fields merged into every request body for this model |
| `health_check.interval_seconds` | int | `10` | Seconds between active health check probes |
| `health_check.timeout_seconds` | int | `3` | Timeout for each health check probe |
| `health_check.cooldown_seconds` | int | `30` | Seconds before an unhealthy backend is retried |
| `request_timeout_seconds` | int | `15` | Timeout for proxied requests to backends |
| `database.dsn` | string | `""` | PostgreSQL connection string (optional; cost records are kept in-memory if omitted) |

## Building and Running

### Prerequisites

- Go 1.15 or later
- PostgreSQL (optional, for cost persistence)

### Build

```bash
go build -o llm-gateway .
```

### Run

```bash
# With default config path (./config.json)
./llm-gateway

# With custom config path
./llm-gateway -config /path/to/config.json
```

### Run with Go

```bash
go run main.go
go run main.go -config /path/to/config.json
```

### PostgreSQL Setup (Optional)

The gateway auto-creates the `cost_records` table and indexes on startup. Just provide a valid DSN:

```bash
# Start PostgreSQL with Docker
docker run -d \
  --name llm-gateway-db \
  -e POSTGRES_USER=gateway \
  -e POSTGRES_PASSWORD=gateway123 \
  -e POSTGRES_DB=llmgateway \
  -p 5432:5432 \
  postgres:16

# Add to config.json:
# "database": {
#   "dsn": "postgres://gateway:gateway123@localhost:5432/llmgateway?sslmode=disable"
# }
```

If the database is unavailable at startup, the gateway logs a warning and runs without persistence — cost records are still tracked in memory.

## API Endpoints

### LLM Proxy Routes

| Method | Path | Description |
|---|---|---|
| POST | `/v1/chat/completions` | OpenAI-compatible chat completions |
| POST | `/v1/messages` | Anthropic Messages API (auto-converted) |
| POST | `/v1/models/{model}:generateContent` | Google AI GenerateContent (auto-converted) |
| POST | `/v1/models/{model}:streamGenerateContent` | Google AI streaming (auto-converted) |
| POST | `/v1beta/models/{model}:generateContent` | Google AI v1beta (auto-converted) |
| POST | `/v1beta/models/{model}:streamGenerateContent` | Google AI v1beta streaming (auto-converted) |

### Dashboard Routes

| Method | Path | Description |
|---|---|---|
| GET | `/ui/costs` | Cost tracking dashboard (auto-refreshes every 30s) |
| GET | `/ui/search` | Search cost records by date range and model |

## Health Check System

The gateway uses a **dual health check** approach:

1. **Active Health Checks** — A background goroutine pings each backend's `health_check_path` at the configured interval. Any response with status < 500 (including 401/403 from unauthenticated endpoints) marks the backend healthy. Status ≥ 500 or connection errors mark it unhealthy.

2. **Passive Health Marking** — When a proxied request to a backend fails (connection error or 5xx), that backend is immediately marked unhealthy. A successful proxied response marks it healthy again.

3. **Cooldown Recovery** — Unhealthy backends are not retried until `cooldown_seconds` has elapsed since they were marked down. After the cooldown, they become eligible for retry by the load balancer.

## Fallback / Failover Chain

The failover system works at two levels:

### Backend-Level Failover
When a request to a backend fails (connection error or 5xx), the load balancer tries the next backend in the pool. The failed backend is marked unhealthy.

### Model-Level Fallback
When a model's entire provider has no healthy backends, the gateway follows the `fallback` chain:

```
Request for "claude-opus-4"
  → provider "main" has no healthy backends
  → fallback to "claude-opus-4-fallback"
  → provider "fallback" has healthy backends → route there
```

Fallback chains can be multiple levels deep. Cycle detection prevents infinite loops. The `backend_model` field rewrites the model name in the request body when falling back to a provider that knows the model by a different name.

## Extra Body Injection

The `extra_body` field in model config merges arbitrary JSON fields into every request body for that model. This is useful for provider-specific parameters that aren't part of the standard API:

```json
{
  "models": {
    "gemini-thinking": {
      "provider": "gemini",
      "extra_body": {
        "thinking": {"type": "enabled", "budget_tokens": 8192},
        "allowed_openai_params": ["thinking"]
      }
    }
  }
}
```

Every request for `gemini-thinking` will have `thinking` and `allowed_openai_params` merged into the request body before forwarding.

## Cost Tracking

When `input_cost_per_token` and `output_cost_per_token` are configured for a model, the gateway tracks costs automatically:

- **Token extraction** — Reads `usage` from non-streaming responses or from the final SSE chunk in streaming responses
- **Real-time logging** — Each request's cost is logged to stdout
- **In-memory storage** — All cost records are kept in memory for the dashboard
- **PostgreSQL persistence** — If configured, records are asynchronously written to the database via a buffered channel
- **Dashboard** — `/ui/costs` shows aggregate costs by model, total spend, and a list of recent requests
- **Search** — `/ui/search` supports filtering by time range (24h, 7d, 30d, or custom dates) and model name with autocomplete

## Running Tests

```bash
go test -v ./...
```

The test suite covers:
- Round-robin request distribution
- Primary-backup preference
- Failover on server errors (5xx) and connection errors
- All-backends-down returns 502
- Request body preservation across retries
- Passive health recovery after cooldown
- API key injection and replacement
- SSE streaming passthrough
- Anthropic tool use round-trip conversion
- Anthropic streaming format conversion
- Image response conversion
- Extra body injection
- Bodiless request handling (returns 400, not 502)

## License

MIT

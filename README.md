# ollama-hook

A transparent HTTP proxy that sits between any Ollama-compatible client and an Ollama
server, exposing lifecycle events during inference as a plugin hook interface. Point
your client at the proxy instead of Ollama directly — it forwards everything
byte-for-byte with zero added latency, while firing hooks at each stage of the request.

```
client → :11435 → ollama-hook → :11434 Ollama
                       │
                 plugin hooks
              (log, metrics, rgb, db, …)
```

Supported endpoints: `POST /api/chat`, `POST /api/generate`, `POST /v1/chat/completions`
(OpenAI-compatible). Everything else passes through untouched via reverse proxy.

## Building

Requires Go 1.26+.

```bash
go build -o ollama-hook .
```

Run the test suite (the race detector is worth it — several hooks manage per-request
concurrent state):

```bash
go test -race ./...
```

## Running

```bash
./ollama-hook
```

With no config file, it listens on `:11435`, proxies to `http://127.0.0.1:11434`, and
runs with only the always-on `log` and `metrics` hooks — `rgb` and `db` are opt-in (see
[Configuration](#configuration)).

CLI flags:

| Flag | Purpose |
|---|---|
| `--config <path>` | Path to `ollama-hook.yaml` (see search order below) |
| `--debug` | Shortcut for `--log-level=debug` |
| `--log-level` | `debug`, `info`, `warn`, `error` — overrides config |
| `--log-format` | `text` or `json` — overrides config |
| `--migrate` | Apply pending database migrations and exit |

`--migrate` reads the database URL from config the same way the `db` plugin does
(`plugins.db.url` / `plugins.db.url_file`, or `DATABASE_URL` / `DATABASE_URL_FILE`), and
works independently of whether the `db` plugin is enabled — useful for provisioning the
schema before turning persistence on.

## Configuration

Config is loaded from `ollama-hook.yaml`, searched in order:

1. `--config <path>`
2. `$OLLAMA_HOOK_CONFIG`
3. `./ollama-hook.yaml`
4. `~/.config/ollama-hook/ollama-hook.yaml`

If none is found, built-in defaults apply (upstream `http://127.0.0.1:11434`, listen
`:11435`, all plugins disabled).

```yaml
upstream: http://127.0.0.1:11434   # OLLAMA_UPSTREAM
listen: :11435                      # PROXY_LISTEN
log_level: info                     # LOG_LEVEL — debug | info | warn | error
log_format: text                    # LOG_FORMAT — text | json

plugins:
  db:
    enabled: false
    url: ""                         # DATABASE_URL
    url_file: ""                    # DATABASE_URL_FILE (systemd-creds path)
  rgb:
    enabled: false
    config_file: ""                 # path to rgb.yaml; searches default dirs if omitted
  webhook:
    enabled: false                  # not yet implemented — see Roadmap
    url: ""
    events: [request_start, request_complete, error]
```

A handful of values can be overridden by environment variable without touching the file
(shown as comments above) — handy for containers and systemd units. Secrets
(`DATABASE_URL`, `PROXY_LOG_TOKEN`) also support `*_FILE` variants for
`systemd-creds`-style credential loading.

If a plugin is `enabled: true` but fails to initialize (bad DSN, unreachable OpenRGB
server, etc.), the proxy refuses to start rather than running silently degraded — check
stderr for the specific error.

### `db` plugin — PostgreSQL conversation logging

Persists every conversation and message asynchronously (batched, non-blocking — a slow
or unavailable database won't add latency to requests).

```bash
./ollama-hook --config ollama-hook.yaml --migrate   # apply schema once
```

Then set `plugins.db.enabled: true` and `plugins.db.url` (or `DATABASE_URL`). Schema is
managed with embedded `golang-migrate` migrations under `migrations/`.

`POST /_proxy/log` lets external callers (anything that isn't going through the proxy's
own streaming path — e.g. a separate automation) log a complete conversation directly.
It requires `plugins.db.enabled` and a bearer token:

```bash
curl -X POST http://localhost:11435/_proxy/log \
  -H "X-Proxy-Log-Token: $PROXY_LOG_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
        "model": "llama3",
        "endpoint": "my_automation",
        "source_type": "automation",
        "query": "what time is it",
        "response": "it is currently...",
        "duration_ms": 850
      }'
```

`source_type`, `query`, `response`, and `duration_ms` are required. An optional
`metadata` object (arbitrary JSON) is stored alongside the conversation for
caller-specific context.

### `rgb` plugin — OpenRGB lighting effects

Drives ARGB hardware in real time as tokens stream in, via a persistent connection to an
[OpenRGB](https://openrgb.org/) server. Set `plugins.rgb.enabled: true` and, optionally,
`plugins.rgb.config_file` pointing at a customized `rgb.yaml` (device indexes, effects,
event→effect mapping). Falls back to `./rgb.yaml`, `~/.config/ollama-hook/rgb.yaml`, or
the embedded defaults if `config_file` is omitted. See `rgb.yaml` in this repo for the
full format and defaults.

## Internal endpoints

| Endpoint | Auth | Purpose |
|---|---|---|
| `GET /_proxy/metrics` | None | Plain-text counters: requests, tokens, errors, RGB queue drops |
| `POST /_proxy/log` | Bearer (`PROXY_LOG_TOKEN`) | External callers log a complete conversation |

## Extending: writing a new plugin

Every hook implements a single interface:

```go
type Hook interface {
    OnRequestStart(reqID, model, endpoint, userPrompt string)
    OnToken(reqID, token string, tokenCount int, elapsed time.Duration)
    OnThinking(reqID, content string)
    OnToolCalls(reqID string, calls json.RawMessage)
    OnRequestComplete(reqID string, duration time.Duration, totalTokens int)
    OnError(reqID string, err error)
}
```

`reqID` correlates every call for a given request, so hooks can track concurrent
requests without cross-talk (see `metrics.go` for the reference pattern: a
`sync.Mutex`-protected `map[reqID]*state`).

Plugins are self-registering and config-gated — `main.go` has no knowledge of `db` or
`rgb` specifically. To add one:

1. Add its config block to `PluginsConfig` in `config.go`.
2. Implement `Hook` in a new file.
3. Register a factory from `init()`:

```go
func init() {
    RegisterPlugin("webhook", newWebhookPlugin)
}

// Return (nil, nil) if disabled — BuildPlugins skips nil hooks.
// Return a non-nil error to abort startup (a plugin the user explicitly
// enabled should fail loudly, not run silently degraded).
func newWebhookPlugin(cfg *Config, logger *slog.Logger) (Hook, error) {
    pc := cfg.Plugins.Webhook
    if !pc.Enabled {
        return nil, nil
    }
    return NewWebhookHook(pc, logger), nil
}
```

That's it — no other files need to change. Two optional interfaces are picked up
automatically if your hook implements them:

- `WriteExternalConversation(ExternalLogRequest) (string, error)` — backs
  `POST /_proxy/log` (see `ExternalLogger` in `proxy.go`)
- `Close() error` — called during graceful shutdown, after the server stops accepting
  requests

This is in-process only: plugins are Go code compiled into the binary, not an
out-of-process/gRPC model.

## Project layout

| File | Responsibility |
|---|---|
| `proxy.go` | Request classification, NDJSON/SSE streaming, tool-call stripping |
| `hooks.go` | `Hook` interface, `MultiHook` fan-out, always-on `log` hook |
| `metrics.go` | Always-on `metrics` hook + `GET /_proxy/metrics` |
| `plugin.go` | Plugin registry (`RegisterPlugin` / `BuildPlugins`) |
| `config.go` | Generic `ollama-hook.yaml` config |
| `database_hook.go` | `db` plugin |
| `openrgb.go` / `rgb_config.go` | `rgb` plugin |
| `capabilities.go` | Caches `/api/show` to know which models support tool calls |
| `migrate.go` / `migrations/` | Embedded `golang-migrate` schema |

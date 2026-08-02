# The skyl gateway

`gateway/` is an optional HTTP service, built on
[go-chi](https://github.com/go-chi/chi), that exposes skyl over the network:
one endpoint that fans out to any configured provider.

## It is a separate module

```
github.com/BAGOMBEKA-JOB-DEV/skyl          ← core library, zero dependencies
github.com/BAGOMBEKA-JOB-DEV/skyl/gateway  ← this, own go.mod, uses chi
```

`go get` on the core library **never** pulls in chi or anything else the server
needs. That is the whole point of the split, and it is recorded in
[ADR-0003](adr/0003-gateway-as-separate-module.md).

The core library is an HTTP *client*; chi routes inbound requests. They solve
opposite problems, so putting chi in the core module would tax every library
user with a router they never call.

## When you want it

- **Non-Go services need models.** A Python worker and a TypeScript frontend can
  both call one endpoint instead of each integrating four vendor SDKs.
- **Keys live in one place.** Application code holds a gateway token, not
  provider credentials. Rotation happens once.
- **One audited egress point.** Every model call in the estate flows through a
  single service you can log, meter, and rate-limit.
- **Swap providers without redeploying callers.** Change gateway config; clients
  don't move.

If you are a Go service calling a model, skip the gateway and import the
library — an extra network hop buys you nothing.

## Run it

```bash
export SKYL_AUTH_TOKEN=$(openssl rand -hex 32)   # required
export ANTHROPIC_API_KEY=sk-ant-...
export OPENAI_API_KEY=sk-...

go run github.com/BAGOMBEKA-JOB-DEV/skyl/gateway/cmd/skyl-gateway
```

Providers are registered from whichever keys are present. Starting with none is
a fatal error rather than a silent no-op.

### Configuration

| Variable | Default | Meaning |
|---|---|---|
| `SKYL_ADDR` | `:8080` | Listen address |
| `SKYL_AUTH_TOKEN` | — | **Required.** Bearer token clients must present |
| `SKYL_DEFAULT_PROVIDER` | first registered | Used when a request omits `provider` |
| `SKYL_REQUEST_TIMEOUT` | `120s` | Per-request upstream timeout |
| `ANTHROPIC_API_KEY` | — | Registers the `anthropic` provider |
| `OPENAI_API_KEY` | — | Registers the `openai` provider |
| `GEMINI_API_KEY` | — | Registers the `gemini` provider |
| `SKYL_COMPAT_NAME` / `_BASE_URL` / `_API_KEY` | — | Registers an OpenAI-compatible provider |

## Endpoints

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/healthz` | Liveness. Unauthenticated |
| `GET` | `/v1/providers` | Registered provider names |
| `GET` | `/v1/models?provider=X` | Live model list from that provider |
| `POST` | `/v1/chat` | Completion |
| `POST` | `/v1/chat/stream` | Completion, streamed as SSE |

### Completion

```bash
curl -sS localhost:8080/v1/chat \
  -H "Authorization: Bearer $SKYL_AUTH_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "provider": "anthropic",
    "model": "claude-opus-5",
    "max_tokens": 512,
    "messages": [{"role": "user", "content": "Hello"}]
  }'
```

```json
{
  "id": "msg_01...",
  "provider": "anthropic",
  "model": "claude-opus-5",
  "text": "Hello! How can I help?",
  "stop_reason": "end_turn",
  "usage": {"input_tokens": 9, "output_tokens": 8}
}
```

### Streaming

`POST /v1/chat/stream` returns `text/event-stream`:

```
data: {"type":"text_delta","text":"Hello"}

data: {"type":"text_delta","text":"!"}

data: {"type":"done","usage":{"input_tokens":9,"output_tokens":8}}
```

The response is flushed per event, and the upstream stream is cancelled as soon
as the client disconnects — a client hanging up must not leave a paid request
running.

## Security

The gateway proxies **paid** APIs, so the failure mode of a misconfiguration is
someone else spending your money.

- **Auth is mandatory.** No `SKYL_AUTH_TOKEN`, no start. There is no flag to
  disable it — an accidental open relay to billed endpoints must not be one
  environment variable away.
- Tokens are compared with `subtle.ConstantTimeCompare`.
- Provider keys are never logged and never returned in an error body.
- Upstream errors are classified and re-emitted with an appropriate status; raw
  provider bodies are not forwarded verbatim, since they can echo request
  content.
- **Run it on a private network.** It is an internal service. If it must face
  the internet, put a reverse proxy with TLS and rate limiting in front.

## Middleware

The chi stack, outermost first:

1. `RequestID` — correlation ID per request
2. `RealIP` — honours `X-Forwarded-For` behind a proxy
3. `Recoverer` — a panicking handler returns 500, not a dead process
4. Structured request logging via `log/slog` — method, path, status, duration
   and request ID only; never headers, never bodies
5. Bearer authentication (skipped only for `/healthz`)

Upstream calls are bounded per request by `SKYL_REQUEST_TIMEOUT` inside each
handler rather than by a router-level timeout, so a streaming response is not
severed mid-generation.

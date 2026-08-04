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

Or in a container, with no keys at all — the sandbox stands in for a provider:

```bash
docker compose up --build
curl -H 'Authorization: Bearer local-dev-token' localhost:8080/v1/providers
```

The image is `gcr.io/distroless/static:nonroot` with a statically linked binary:
no shell, no package manager, configuration entirely from the environment, logs
to stdout. See the [runbook](#runbook) for deploying it.

### Configuration

Everything is an environment variable; there are no flags and no config file.
A malformed value is a **startup failure**, never a silently ignored setting —
the process logs which variable was wrong and exits 1.

**Listener and auth**

| Variable | Default | Meaning |
|---|---|---|
| `SKYL_ADDR` | `:8080` | Listen address |
| `SKYL_AUTH_TOKEN` | — | **Required.** Bearer token clients must present |
| `SKYL_AUTH_TOKENS` | — | Extra accepted tokens as `label:token,label:token`. This is how you rotate without downtime. The **label** appears in logs and metrics; the token never does — so do not name a token after its own value |
| `SKYL_ALLOWED_ORIGINS` | — | Comma-separated origins to enable CORS for. Empty disables it, which is right for a server holding API keys |

**Providers** — a provider is registered for each key present.

| Variable | Registers |
|---|---|
| `ANTHROPIC_API_KEY` | `anthropic` |
| `OPENAI_API_KEY` | `openai` |
| `GEMINI_API_KEY` | `gemini` |
| `SKYL_COMPAT_BASE_URL` | an OpenAI-compatible provider — **this variable alone triggers registration** |
| `SKYL_COMPAT_NAME` | its name, default `compat` |
| `SKYL_COMPAT_API_KEY` | its key; may be empty for local runtimes |
| `SKYL_DEFAULT_PROVIDER` | used when a request omits `provider`; defaults to the alphabetically first registered |

**Behaviour**

| Variable | Default | Meaning |
|---|---|---|
| `SKYL_REQUEST_TIMEOUT` | `120s` | Per-request upstream timeout. **Not applied to `/v1/chat/stream`** |
| `SKYL_MAX_CONCURRENT` | unlimited | In-flight request cap. A concurrency gate, **not** a rate limit |
| `SKYL_HEARTBEAT_INTERVAL` | `15s` | SSE keep-alive frames on an idle stream; negative disables |
| `SKYL_METRICS` | `false` | Serve Prometheus metrics at `/metrics` |
| `SKYL_INCLUDE_RAW` | `false` | Echo each provider's raw body in responses. Off because raw bodies can carry request content back to a caller who should not see it |

**Retry and timeouts**, applied to every provider client. Defaults come from the
library; set these when the gateway sits behind a caller that is already
retrying, because the two multiply.

| Variable | Default | Meaning |
|---|---|---|
| `SKYL_MAX_RETRIES` | `3` | Retries after the first attempt |
| `SKYL_RETRY_BASE_DELAY` | `500ms` | Backoff base |
| `SKYL_RETRY_MAX_DELAY` | `30s` | Backoff ceiling |
| `SKYL_RETRY_AFTER_CAP` | `5m` | Longest a provider's `Retry-After` may hold a request |
| `SKYL_ATTEMPT_TIMEOUT` | `10m` | Per-attempt timeout. On `/v1/chat/stream` this is the **only** bound |

`SKYL_RETRY_BASE_DELAY` and `SKYL_RETRY_MAX_DELAY` are one setting: set either
and the other keeps its library default.

## Endpoints

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/healthz` | no | **Liveness.** Stays green while draining — see the [runbook](#runbook) |
| `GET` | `/readyz` | no | **Readiness.** `503` once shutdown begins |
| any | `/metrics` | no | Prometheus metrics. Only registered when `SKYL_METRICS=true` |
| `GET` | `/v1/providers` | yes | Registered provider names |
| `GET` | `/v1/models?provider=X` | yes | Live model list from that provider |
| `POST` | `/v1/chat` | yes | Completion |
| `POST` | `/v1/chat/stream` | yes | Completion, streamed as SSE |

`/healthz`, `/readyz` and `/metrics` are unauthenticated so an orchestrator and
a scraper need no credential. `/metrics` carries no prompt content, but it does
expose model names, token volumes and error rates for your whole estate — keep
it on a private network.

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
2. Request-ID echo — returns it as `X-Request-Id`, so a caller has something to
   quote when reporting a problem. chi generates the ID into the context only
3. `Recoverer` — a panicking handler returns 500, not a dead process
4. Structured request logging via `log/slog` — method, path, status, bytes,
   duration, request ID and caller label; never headers, never bodies, and not
   the query string
5. CORS, when `SKYL_ALLOWED_ORIGINS` is set. It runs **before** authentication
   because a preflight `OPTIONS` carries no `Authorization` header
6. Bearer authentication — skipped for `/healthz`, `/readyz` and `/metrics`
7. Concurrency limiting, when `SKYL_MAX_CONCURRENT` is set. It sits **inside**
   authentication so unauthenticated traffic cannot consume the budget

chi's `RealIP` is deliberately **not** in the stack. It rewrites
`r.RemoteAddr` from `X-Forwarded-For` / `True-Client-IP` / `X-Real-IP`
regardless of whether your infrastructure sets them, so any client can claim
any address (GHSA-3fxj-6jh8-hvhx). The gateway needs no client IP, so
`RemoteAddr` is left as the real peer address. If you need the originating IP,
read it from a header your own trusted proxy is known to set.

Upstream calls are bounded per request by `SKYL_REQUEST_TIMEOUT` inside each
handler rather than by a router-level timeout, so a streaming response is not
severed mid-generation.

---

## Runbook

Everything below was verified against a running binary, not inferred from the
code.

### Probes: `/healthz` and `/readyz` are not the same question

| | `/healthz` | `/readyz` |
|---|---|---|
| Question | is the process alive? | should traffic come here? |
| While draining | **`200`** | **`503`** |
| Wire it to | the restart policy | the load balancer |

**Liveness stays green while draining, deliberately.** The process is alive and
finishing work. A liveness probe that failed during drain would have the
orchestrator SIGKILL the pod mid-request, which is the opposite of a graceful
shutdown.

Never point your restart probe at `/readyz`.

Neither probe checks upstream providers. A provider outage does not make the
gateway unready — there is nothing useful to fail over to, and flapping
readiness on a provider blip would take the whole fleet out of rotation.

### Shutdown, step by step

On `SIGTERM` or `SIGINT`:

| | Elapsed | What happens |
|---|---|---|
| 1 | `0s` | Logs `draining`. **`/readyz` starts returning 503 immediately.** `/healthz` stays 200 |
| 2 | `0–2s` | `drainDelay` — the window for your load balancer to notice and stop routing here. The listener is still open and still accepting |
| 3 | `2s` | Logs `shutting down`. The listener closes. In-flight requests continue |
| 4 | up to `32s` | `shutdownGrace` of 30s for in-flight work to finish |
| 5 | | Logs `shutdown complete`, exits **0** |

**Worst case is ~32 seconds.** Set `terminationGracePeriodSeconds` to at least
`40`, or your runtime SIGKILLs the process mid-drain and you lose the graceful
part entirely.

The `drainDelay` exists because a load balancer learns about readiness by
polling. Closing the listener the instant we decide to stop means requests
already in flight toward this instance hit a closed socket — a connection
refused, which looks exactly like a crash.

### Exit codes

- **`0`** — normal, including when the grace period expired with work still in
  flight.
- **`1`** — the only non-zero code. Startup validation failed, or the listener
  could not bind.

**Alert on the log line, not the exit code.** A stream still running at the
grace deadline logs:

```
{"level":"WARN","msg":"grace period expired with requests still in flight; closing anyway","grace":30000000000}
```

and still exits 0 — otherwise every deploy during a long generation would look
like a crash in your dashboards. That WARN is the signal worth alerting on.

### Streams during shutdown

A stream still generating at the grace deadline is **force-closed**. The client
sees a truncated SSE stream with no terminal `done` event, and the upstream call
was already paid for.

Two consequences:

- **Clients must treat a stream that ends without `done` as retryable.** skyl's
  own client does this correctly — it reports *"stream ended without a terminal
  event; the response is truncated"* rather than a short but complete answer.
- **Schedule deploys away from long generations**, or raise the grace period,
  if truncation is expensive for you.

### Why the gateway refuses to start

Every one of these exits 1 with a message naming the problem:

- no providers registered — set at least one provider API key;
- no `SKYL_AUTH_TOKEN` — *"refusing to start an open relay to paid APIs"*.
  There is no flag to disable authentication;
- `SKYL_DEFAULT_PROVIDER` names a provider that is not registered;
- a token in `SKYL_AUTH_TOKENS` has an empty value;
- any malformed duration, integer or boolean, with the variable named;
- the listen address cannot be bound.

The gateway never starts with a partially applied configuration.

### Token rotation

`SKYL_AUTH_TOKENS` accepts several `label:token` pairs, all valid at once. To
rotate without downtime: add the new token, let callers migrate, remove the old
one. There is no reload signal — `SIGHUP` is not handled — so each step is a
restart, which is what the drain sequence above is for.

⚠️ **Labels are attribution, not authorisation.** Every valid token reaches the
same providers, the same keys and the same quota. See
[threat-model.md](threat-model.md#what-an-authenticated-gateway-caller-can-do)
before issuing a token to a party you do not fully trust.

### Scaling and capacity

- **The gateway is stateless.** Run as many as you like behind a load balancer;
  nothing is shared between them.
- **Sizing is driven by concurrency, not CPU.** Each in-flight request holds a
  goroutine and an upstream connection for its whole duration, which for a model
  call is seconds to minutes. Memory is roughly request size plus response size
  per in-flight request.
- ⚠️ **Upstream connection reuse is capped at 10 idle connections per host.**
  Beyond that, every request pays a fresh TLS handshake. This is Go's transport
  default and skyl does not currently expose it — with high concurrency to one
  provider, this is the first thing to notice in latency traces.
- **Set `SKYL_MAX_CONCURRENT`** to something your provider quota tolerates. It
  returns 429 when full, which is a better failure than an unbounded fan-out to
  a paid API.
- **There is no rate limiting.** Put a reverse proxy in front if you need it.

### TLS

The gateway serves **plain HTTP**. Terminate TLS at a reverse proxy or load
balancer. It holds provider API keys and must never be directly exposed.

If you terminate at nginx, note the gateway already sets
`X-Accel-Buffering: no` on streams — without it, nginx buffers the whole
response and the streaming endpoint stops streaming.

### Metrics

With `SKYL_METRICS=true`, `/metrics` serves the OpenTelemetry GenAI conventions
in Prometheus format:

- `gen_ai_client_operation_duration_seconds` — histogram, labelled by operation,
  provider, request model and response model;
- `gen_ai_client_token_usage` — histogram, labelled additionally by token
  direction (`input`/`output`).

Same vocabulary a Go service importing skyl directly emits, so one dashboard
covers both. No prompt content reaches a metric label.

Useful alerts: error rate by `error_type`, p99 duration by `gen_ai_request_model`,
and token spend by model — the last is the one that catches a caller switching to
an expensive model.

### Troubleshooting

| Symptom | Likely cause |
|---|---|
| Exits 1 immediately, `no providers registered` | No provider API key in the environment |
| Exits 1, `an auth token is required` | `SKYL_AUTH_TOKEN` unset. This is not optional |
| Exits 1 naming a variable | Malformed value; the message names it |
| `401` on every request | Token mismatch. The header must be exactly `Authorization: Bearer <token>` |
| `404 unknown provider` | The provider was never registered — its API key is missing |
| `502` with `kind: auth` | **Your** provider key was rejected, not the caller's token |
| Streams arrive all at once | A proxy is buffering. The gateway sets `X-Accel-Buffering: no`; check intermediaries |
| Streams cut after ~60s | An idle intermediary is reaping. Lower `SKYL_HEARTBEAT_INTERVAL` |
| `429` from the gateway itself | `SKYL_MAX_CONCURRENT` is full |
| Deploys take 30s and log a WARN | A stream was in flight at the deadline. Expected; raise the grace period if costly |
| Latency spikes under load | Idle-connection cap (10/host) forcing fresh TLS handshakes |

### Deployment checklist

- [ ] `SKYL_AUTH_TOKEN` generated with `openssl rand -hex 32`, from a secret store
- [ ] Provider keys from a secret store, never an image layer
- [ ] TLS terminated in front; the gateway is not directly exposed
- [ ] `/metrics` unreachable from outside the cluster
- [ ] Liveness → `/healthz`, readiness → `/readyz`
- [ ] `terminationGracePeriodSeconds` ≥ 40
- [ ] `SKYL_MAX_CONCURRENT` set to match provider quota
- [ ] Alerting on the grace-expiry WARN and on `error_type` rate
- [ ] Read [data-handling.md](data-handling.md) — upstream error messages can
      carry prompt fragments into your logs

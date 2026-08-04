# Container image for skyl-gateway.
#
#	docker build -t skyl-gateway .
#	docker run --rm -p 8080:8080 \
#	  -e SKYL_AUTH_TOKEN=... -e OPENAI_API_KEY=... skyl-gateway
#
# Three things about this repository shape the build, and each one costs an
# afternoon if you meet it by surprise:
#
#  1. The build context must be the REPOSITORY ROOT, not gateway/. The gateway
#     module resolves its siblings through `replace => ..`, so a context rooted
#     at gateway/ cannot see them.
#  2. GOWORK is set to off. Inside the container the gateway is the main module,
#     so its own replace directives apply and go.work is unnecessary — and a
#     stray go.work in the context would silently change resolution.
#  3. There is NO root go.sum. The root module has zero external dependencies,
#     so `COPY go.sum ./` would fail. Only three modules have one.

# --- build -------------------------------------------------------------------
FROM golang:1.26 AS build

ENV CGO_ENABLED=0 GOWORK=off GOTOOLCHAIN=go1.26.0

WORKDIR /src

# Manifests first, so a source-only change does not re-download the module
# cache. Note the deliberate absence of a root go.sum, per (3) above.
COPY go.mod ./
COPY provider/anthropic/go.mod provider/anthropic/go.sum ./provider/anthropic/
COPY otel/go.mod otel/go.sum ./otel/
COPY gateway/go.mod gateway/go.sum ./gateway/

# The replace targets must exist before `go mod download` will resolve them,
# even though they hold no sources yet.
RUN mkdir -p internal provider/openai provider/gemini provider/openaicompat \
    && cd gateway && go mod download

# Only what the gateway binary actually imports. cmd/skyl-sandbox, docs,
# scripts and every _test.go file are deliberately absent.
COPY *.go ./
COPY internal/ ./internal/
COPY provider/ ./provider/
COPY otel/ ./otel/
COPY gateway/ ./gateway/

# Trimpath keeps build-host paths out of the binary; the ldflags drop DWARF and
# the symbol table, which is most of the size.
RUN cd gateway && go build -trimpath -ldflags="-s -w" -o /out/skyl-gateway ./cmd/skyl-gateway

# --- runtime -----------------------------------------------------------------
#
# distroless/static rather than scratch: it already carries the CA bundle, which
# is not optional here. The gateway makes outbound HTTPS calls to every
# provider, and a rejected certificate is deliberately NON-RETRYABLE — a missing
# bundle fails the first request immediately rather than degrading.
#
# It also provides /etc/passwd for the nonroot user and tzdata. Nothing here is
# cgo-linked and nothing reads a file at runtime: configuration is entirely
# environment, and logs go to stdout.
FROM gcr.io/distroless/static:nonroot

COPY --from=build /out/skyl-gateway /usr/local/bin/skyl-gateway

EXPOSE 8080
USER nonroot:nonroot

# No shell in this image, so there is no HEALTHCHECK — probe /healthz and
# /readyz from your orchestrator instead. They mean different things; see
# docs/gateway.md.
ENTRYPOINT ["/usr/local/bin/skyl-gateway"]

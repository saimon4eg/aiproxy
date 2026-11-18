# ── Stage 1: build ──────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /aiproxy .

# ── Stage 2: run ───────────────────────────────────────────────────────────
# glibc, not musl: Claude Code ships a platform-specific native binary selected
# from optionalDependencies, and an alpine runtime resolves the -musl variant and
# then fails at launch with a "musl dynamic loader missing" style error that
# surfaces as an opaque 500 instead of a build failure.
FROM debian:bookworm-slim

# The CLI version is pinned on purpose: the transport depends on version-sensitive
# interfaces (stream-json replay acknowledgement, --include-partial-messages,
# --system-prompt-file, --max-turns). Bump CLAUDE_CLI_VERSION deliberately and
# re-run the transport tests. "latest" is not used: npm's resolution in this base
# image can land on an old build that silently lacks those flags.
ARG CLAUDE_CLI_VERSION=2.1.311

# tini reaps the CLI's grandchildren: every request spawns `claude`, which
# spawns node, and node as PID 1 would never reap them.
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl tzdata gnupg xz-utils tini \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    # The official CLI, unmodified. On the subscription path it is the only
    # thing that talks to Anthropic: the request identity must come from the
    # real client, never from aiproxy.
    && npm install -g "@anthropic-ai/claude-code@${CLAUDE_CLI_VERSION}" \
    && npm cache clean --force \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /aiproxy /usr/local/bin/aiproxy
COPY --from=builder /app/providers.json /app/providers.json
EXPOSE 7777
ENV COPILOT2API_HOST=0.0.0.0
ENV COPILOT2API_PORT=7777
HEALTHCHECK --interval=30s --timeout=10s --retries=3 \
  CMD curl -sf http://localhost:7777/health || exit 1
WORKDIR /app
RUN useradd --create-home --shell /bin/false aiproxy \
    && mkdir -p /home/aiproxy/.config/copilot2api \
    && chown -R aiproxy:aiproxy /home/aiproxy /app
USER aiproxy
ENV HOME=/home/aiproxy
ENV COPILOT2API_TOKEN_DIR=/home/aiproxy/.config/copilot2api
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/aiproxy"]

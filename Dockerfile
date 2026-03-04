# ── Stage 1: build ──────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /aiproxy .

# ── Stage 2: run ───────────────────────────────────────────────────────────
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl tzdata python3 python3-venv \
    && rm -rf /var/lib/apt/lists/*
RUN python3 -m venv /opt/venv && \
    /opt/venv/bin/pip install --no-cache-dir martian-linguafranca
ENV PATH="/opt/venv/bin:$PATH"
COPY --from=builder /aiproxy /usr/local/bin/aiproxy
COPY --from=builder /app/scripts/ /app/scripts/
COPY --from=builder /app/providers.json /app/providers.json
EXPOSE 7777
ENV COPILOT2API_HOST=0.0.0.0
ENV COPILOT2API_PORT=7777
ENV LINGUAFRANCA_BRIDGE=/app/scripts/linguafranca_bridge.py
HEALTHCHECK --interval=30s --timeout=10s --retries=3 \
  CMD curl -sf http://localhost:7777/health || exit 1
WORKDIR /app
RUN useradd --create-home --shell /bin/false aiproxy \
    && mkdir -p /home/aiproxy/.config/copilot2api \
    && chown -R aiproxy:aiproxy /home/aiproxy /app
USER aiproxy
ENV HOME=/home/aiproxy
ENV COPILOT2API_TOKEN_DIR=/home/aiproxy/.config/copilot2api
ENTRYPOINT ["/usr/local/bin/aiproxy"]

# Build the static binary. CGO stays off: the SQLite driver is pure Go, so the
# runtime image needs no libc beyond what Debian already has.
FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/spool ./cmd/spool

FROM debian:bookworm-slim

# Pin the CLI. The result classifier and the login parser both scrape CLI
# output, so upgrades are deliberate: re-run backend/spike against the new
# version, bump this, and watch the auth metrics afterwards (design §3.2).
# This is the only pin. Release builds and the deploy/ scripts inherit it.
ARG CLAUDE_CODE_VERSION=2.1.282
ARG NODE_MAJOR=22

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl git ripgrep tini \
    && curl -fsSL https://deb.nodesource.com/setup_${NODE_MAJOR}.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} \
    && npm cache clean --force \
    && apt-get purge -y curl && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/*

# Non-root: /data holds a live credential.
RUN useradd --create-home --uid 10001 spool \
    && mkdir -p /data /etc/spool \
    && chown -R spool:spool /data

COPY --from=build /out/spool /usr/local/bin/spool

USER spool
WORKDIR /home/spool
ENV CLAUDE_CONFIG_DIR=/data/claude \
    DISABLE_AUTOUPDATER=1
VOLUME ["/data"]
EXPOSE 8080

# tini reaps the claude subprocesses so a killed run cannot leave zombies.
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["spool", "--config", "/etc/spool/config.yaml", "--queues", "/etc/spool/queues.yaml"]

# The spike container: a bare Debian with a pinned Claude Code CLI and nothing
# of Spool in it. The point is to observe the CLI's real behaviour, so keep
# this as close to the production image's CLI setup as possible and no closer.
FROM debian:bookworm-slim

ARG CLAUDE_CODE_VERSION=latest
ARG NODE_MAJOR=22

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl git ripgrep jq procps \
    && curl -fsSL https://deb.nodesource.com/setup_${NODE_MAJOR}.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} \
    && npm cache clean --force \
    && rm -rf /var/lib/apt/lists/*

RUN useradd --create-home --uid 10001 spike \
    && mkdir -p /data/claude /out /probes \
    && chown -R spike:spike /data /out

USER spike
WORKDIR /home/spike

# Mirrors the executor's environment exactly (internal/claudecli/invocation.go).
# Note what is absent: ANTHROPIC_API_KEY and CLAUDE_CODE_OAUTH_TOKEN, either of
# which would override the login and disable the skills and connectors this
# spike exists to verify.
ENV CLAUDE_CONFIG_DIR=/data/claude \
    CLAUDE_CODE_SYNC_SKILLS=1 \
    DISABLE_AUTOUPDATER=1

CMD ["sleep", "infinity"]

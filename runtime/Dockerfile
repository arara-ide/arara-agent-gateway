FROM node:22-bookworm-slim

ARG CLAUDE_CODE_VERSION=latest
ARG CODEX_VERSION=latest

ENV DEBIAN_FRONTEND=noninteractive \
    AGY_CLI_DISABLE_AUTO_UPDATE=true \
    DISABLE_AUTOUPDATER=1 \
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
    NPM_CONFIG_UPDATE_NOTIFIER=false

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl git procps ripgrep openssh-client jq less \
 && rm -rf /var/lib/apt/lists/*

RUN npm install -g --include=optional "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
 && npm install -g --include=optional "@openai/codex@${CODEX_VERSION}" \
 && npm cache clean --force \
 && claude --version \
 && codex --version

RUN curl -fsSL https://antigravity.google/cli/install.sh -o /tmp/agy-install.sh \
 && HOME=/tmp/agy-home bash /tmp/agy-install.sh --dir /usr/local/bin \
 && rm -rf /tmp/agy-install.sh /tmp/agy-home \
 && agy --version

RUN groupmod -n agent node \
 && usermod -l agent -d /home/agent -m node \
 && mkdir -p /home/agent/.codex /home/agent/work \
 && printf 'cli_auth_credentials_store = "file"\n' > /home/agent/.codex/config.toml \
 && chown -R 1000:1000 /home/agent

COPY runtime/bin/aag-run runtime/bin/aag-kill /usr/local/bin/

USER 1000:1000
WORKDIR /home/agent
ENV HOME=/home/agent
CMD ["sleep", "infinity"]

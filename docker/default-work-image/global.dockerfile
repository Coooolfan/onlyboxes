FROM ubuntu:24.04

ARG TARGETARCH

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      python3 \
      python3-pip \
      python3-venv \
      curl \
      wget \
      git \
      ca-certificates \
      jq \
      ripgrep \
      fd-find \
      tree \
      file \
      less \
      unzip \
      zip \
      procps \
      sqlite3; \
    rm -rf /var/lib/apt/lists/*

RUN set -eux; \
    ln -sf /usr/bin/python3 /usr/local/bin/python; \
    python3 -m pip install --no-cache-dir --break-system-packages \
      python-docx \
      pypdf \
      openpyxl \
      Pillow

RUN set -eux; \
    arch="${TARGETARCH:-$(dpkg --print-architecture)}"; \
    case "${arch}" in \
      amd64) agent_browser_url="https://github.com/vercel-labs/agent-browser/releases/download/v0.24.1/agent-browser-linux-x64" ;; \
      arm64) agent_browser_url="https://github.com/vercel-labs/agent-browser/releases/download/v0.24.1/agent-browser-linux-arm64" ;; \
      *) echo "unsupported TARGETARCH: ${arch}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "${agent_browser_url}" -o /usr/local/bin/agent-browser; \
    chmod 755 /usr/local/bin/agent-browser

RUN bash -o pipefail -c 'curl -fsSL https://pkg.lightpanda.io/install.sh | bash'

WORKDIR /tmp

ENV PIP_BREAK_SYSTEM_PACKAGES=1 \
    PIP_ROOT_USER_ACTION=ignore \
    AGENT_BROWSER_ENGINE=lightpanda

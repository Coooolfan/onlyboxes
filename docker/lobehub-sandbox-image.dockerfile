FROM ubuntu:24.04

ARG TARGETARCH

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      bash \
      python3 \
      python3-pip \
      python3-venv \
      curl \
      wget \
      git \
      gh \
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
      sqlite3 \
      build-essential \
      ffmpeg \
      fonts-noto-cjk \
      fonts-noto-color-emoji \
      libreoffice \
      pandoc \
      poppler-utils; \
    rm -rf /var/lib/apt/lists/*

RUN set -eux; \
    ln -sf /usr/bin/python3 /usr/local/bin/python; \
    python3 -m pip install --no-cache-dir --break-system-packages \
      aiofiles \
      anyio \
      fastapi \
      matplotlib \
      numpy \
      opencv-python-headless \
      pandas \
      plotly \
      pydantic \
      python-docx \
      python-dotenv \
      PyPDF2 \
      pypdf \
      pytest \
      openpyxl \
      Pillow \
      reportlab \
      scipy \
      scikit-learn \
      toml \
      uvicorn \
      xlrd \
      pyyaml

RUN set -eux; \
    curl -fsSL https://deb.nodesource.com/setup_24.x | bash -; \
    apt-get install -y --no-install-recommends nodejs; \
    corepack enable; \
    npm install -g \
      @anthropic-ai/claude-code \
      @lobehub/cli \
      @openai/codex \
      tsx \
      @marp-team/marp-cli; \
    rm -rf /var/lib/apt/lists/* /root/.npm

RUN set -eux; \
    BUN_INSTALL=/usr/local/bun bash -o pipefail -c 'curl -fsSL https://bun.sh/install | bash'; \
    ln -sf /usr/local/bun/bin/bun /usr/local/bin/bun; \
    ln -sf /usr/local/bun/bin/bunx /usr/local/bin/bunx

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

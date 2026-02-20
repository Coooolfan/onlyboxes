FROM ubuntu:24.04

RUN apt-get update && apt-get install -y --no-install-recommends \
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
    sqlite3 \
    && rm -rf /var/lib/apt/lists/*

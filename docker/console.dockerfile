FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

ARG TARGETARCH
COPY --chmod=755 onlyboxes-console-${TARGETARCH} /usr/local/bin/onlyboxes-console

EXPOSE 8089 50051

ENTRYPOINT ["/usr/local/bin/onlyboxes-console"]

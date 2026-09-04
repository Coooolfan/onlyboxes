#!/usr/bin/env bash
# scripts/dev.sh 的内部公共预览适配器。开发者应通过 dev.sh 调用本脚本。
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
STATE_DIR="$SCRIPT_DIR/.dev"
DEV_ENV="${ONLYBOXES_DEV_ENV:-$SCRIPT_DIR/dev.env}"
PUBLIC_ENV="$SCRIPT_DIR/public-preview.env"
CREDS_FILE="$STATE_DIR/console-creds.json"
WORKER_ENV="$STATE_DIR/worker-docker.env"
WORKER_BINARY="$STATE_DIR/worker-docker-linux"
NGINX_CONFIG="$STATE_DIR/public-preview-nginx.conf"
NGINX_TEMPLATE="$SCRIPT_DIR/public-preview.nginx.conf.template"
COOKIE_FILE="$STATE_DIR/public-preview-console-cookie.txt"

# shellcheck source=/dev/null
[ -f "$PUBLIC_ENV" ] && . "$PUBLIC_ENV"

HTTP_PORT="${PUBLIC_PREVIEW_HTTP_PORT:-80}"
DOCKER_NETWORK="${PUBLIC_PREVIEW_DOCKER_NETWORK:-onlyboxes-preview-dev}"
DOCKER_SUBNET="${PUBLIC_PREVIEW_DOCKER_SUBNET:-172.30.250.0/24}"
DOCKER_GATEWAY="${PUBLIC_PREVIEW_DOCKER_GATEWAY:-172.30.250.1}"
NGINX_IMAGE="${PUBLIC_PREVIEW_NGINX_IMAGE:-nginx:1.26-alpine}"
WORKER_IMAGE="${PUBLIC_PREVIEW_WORKER_IMAGE:-docker:29-cli}"
WORKER_LEASE_MAX_SEC="${PUBLIC_PREVIEW_WORKER_LEASE_MAX_SEC:-2147483647}"
NGINX_CONTAINER="onlyboxes-public-preview-nginx"
WORKER_CONTAINER="onlyboxes-public-preview-worker"

die() {
    echo "错误：$*" >&2
    exit 1
}

require_command() {
    command -v "$1" >/dev/null 2>&1 || die "未找到 $1"
}

dev_env_value() {
    local key="$1"
    [ -f "$DEV_ENV" ] || return 0
    sed -n "s/^[[:space:]]*${key}=//p" "$DEV_ENV" |
        tail -n 1 |
        sed -e 's/^["'\'']//' -e 's/["'\'']$//'
}

addr_port() {
    printf '%s' "${1##*:}"
}

console_http_port() {
    local addr
    addr="$(dev_env_value CONSOLE_HTTP_ADDR)"
    if [ -n "$addr" ]; then addr_port "$addr"; else printf '8089'; fi
}

console_grpc_port() {
    local addr
    addr="$(dev_env_value CONSOLE_GRPC_ADDR)"
    if [ -n "$addr" ]; then addr_port "$addr"; else printf '50051'; fi
}

worker_runner() {
    local configured="${ONLYBOXES_WORKER_DOCKER_RUNNER:-}"
    if [ -n "$configured" ]; then
        case "$configured" in
            orb | native) printf '%s' "$configured" ;;
            *) die "ONLYBOXES_WORKER_DOCKER_RUNNER 只能是 orb 或 native" ;;
        esac
        return
    fi
    if [ "$(uname -s)" = "Darwin" ]; then printf 'orb'; else printf 'native'; fi
}

docker_ready() {
    docker info >/dev/null 2>&1
}

require_docker() {
    require_command docker
    docker_ready || die "Docker/OrbStack 未启动或当前用户无法访问 Docker API"
}

ensure_network() {
    require_docker
    if docker network inspect "$DOCKER_NETWORK" >/dev/null 2>&1; then
        local subnet gateway
        subnet="$(docker network inspect "$DOCKER_NETWORK" --format '{{(index .IPAM.Config 0).Subnet}}')"
        gateway="$(docker network inspect "$DOCKER_NETWORK" --format '{{(index .IPAM.Config 0).Gateway}}')"
        [ "$subnet" = "$DOCKER_SUBNET" ] || die "Docker 网络 ${DOCKER_NETWORK} 的 subnet 是 ${subnet}，期望 $DOCKER_SUBNET"
        [ "$gateway" = "$DOCKER_GATEWAY" ] || die "Docker 网络 ${DOCKER_NETWORK} 的 gateway 是 ${gateway}，期望 $DOCKER_GATEWAY"
        return
    fi
    docker network create --driver bridge --subnet "$DOCKER_SUBNET" --gateway "$DOCKER_GATEWAY" "$DOCKER_NETWORK" >/dev/null
    echo "• 已创建公共预览网络 ${DOCKER_NETWORK}（${DOCKER_SUBNET}）"
}

wait_console() {
    require_command curl
    local port code
    port="$(console_http_port)"
    for _ in $(seq 1 60); do
        code="$(curl -sS -o /dev/null -w '%{http_code}' "http://127.0.0.1:${port}/api/v1/console/session" 2>/dev/null || true)"
        [ "$code" != "000" ] && return
        sleep 0.5
    done
    die "Console 未就绪（http://127.0.0.1:${port}）"
}

credential_field() {
    local key="$1" value
    [ -f "$CREDS_FILE" ] || return 0
    value="$(jq -r --arg key "$key" '.[$key] // empty' "$CREDS_FILE" 2>/dev/null || true)"
    if [ -z "$value" ]; then
        value="$(sed -n "s/.*\"${key}\":\"\([^\"]*\)\".*/\1/p" "$CREDS_FILE")"
    fi
    printf '%s' "$value"
}

AUTH_KIND=""
AUTH_VALUE=""

authenticate_console() {
    require_command curl
    require_command jq
    wait_console

    local port candidate username password body
    port="$(console_http_port)"
    for candidate in "${ONLYBOXES_DEV_CONSOLE_API_KEY:-}" "$(dev_env_value CONSOLE_INITIAL_ADMIN_API_KEY)"; do
        [ -n "$candidate" ] || continue
        if curl -fsS -H "Authorization: Bearer $candidate" "http://127.0.0.1:${port}/api/v1/console/session" >/dev/null 2>&1; then
            AUTH_KIND="bearer"
            AUTH_VALUE="$candidate"
            return
        fi
    done

    username="$(credential_field username)"
    password="$(credential_field password)"
    [ -n "$username" ] && [ -n "$password" ] || die "没有可用的 Console API Key 或管理员凭据；先执行 scripts/dev.sh creds"
    mkdir -p "$STATE_DIR"
    body="$(jq -nc --arg username "$username" --arg password "$password" '{username:$username,password:$password}')"
    curl -fsS -c "$COOKIE_FILE" -H 'Content-Type: application/json' -d "$body" "http://127.0.0.1:${port}/api/v1/console/login" >/dev/null ||
        die "Console 管理员登录失败"
    chmod 600 "$COOKIE_FILE"
    AUTH_KIND="cookie"
    AUTH_VALUE="$COOKIE_FILE"
}

console_api() {
    local port path arg_count
    port="$(console_http_port)"
    arg_count="$#"
    [ "$arg_count" -gt 0 ] || die "Console API 缺少路径"
    path="${!arg_count}"
    set -- "${@:1:$((arg_count - 1))}"
    if [ "$AUTH_KIND" = "bearer" ]; then
        curl -fsS -H "Authorization: Bearer $AUTH_VALUE" "$@" "http://127.0.0.1:${port}${path}"
    else
        curl -fsS -b "$AUTH_VALUE" "$@" "http://127.0.0.1:${port}${path}"
    fi
}

env_value() {
    local file="$1" key="$2"
    [ -f "$file" ] || return 0
    sed -n "s/^[[:space:]]*${key}=//p" "$file" | tail -n 1 | sed -e 's/^["'\'']//' -e 's/["'\'']$//'
}

upsert_env() {
    local file="$1" key="$2" value="$3" tmp
    tmp="$(mktemp "$STATE_DIR/.worker-env.XXXXXX")"
    awk -v key="$key" -v value="$value" '
        BEGIN { found = 0 }
        index($0, key "=") == 1 { print key "=" value; found = 1; next }
        { print }
        END { if (!found) print key "=" value }
    ' "$file" >"$tmp"
    chmod 600 "$tmp"
    mv "$tmp" "$file"
}

write_worker_env() {
    local node_id="$1" secret="$2" target tmp
    if [ "$(worker_runner)" = "orb" ]; then
        target="0.250.250.254:$(console_grpc_port)"
    else
        target="127.0.0.1:$(console_grpc_port)"
    fi
    tmp="$(mktemp "$STATE_DIR/.worker-env.XXXXXX")"
    {
        printf 'WORKER_ID=%s\n' "$node_id"
        printf 'WORKER_SECRET=%s\n' "$secret"
        printf 'WORKER_CONSOLE_GRPC_TARGET=%s\n' "$target"
        printf 'WORKER_CONSOLE_INSECURE=true\n'
        printf 'WORKER_NODE_NAME=local-proxy-docker\n'
        printf 'WORKER_HEARTBEAT_INTERVAL_SEC=5\n'
        printf 'WORKER_HEARTBEAT_JITTER_PCT=20\n'
        printf 'WORKER_TERMINAL_LEASE_MAX_SEC=%s\n' "$WORKER_LEASE_MAX_SEC"
        printf 'WORKER_PROXY_ENABLED=true\n'
        printf 'WORKER_PROXY_LISTEN_ADDR=:8091\n'
        printf 'WORKER_PROXY_ADVERTISE_ADDR=%s:8091\n' "$DOCKER_GATEWAY"
        printf 'WORKER_LOG_LEVEL=info\n'
        printf 'WORKER_LOG_FORMAT=json\n'
    } >"$tmp"
    chmod 600 "$tmp"
    mv "$tmp" "$WORKER_ENV"
}

sync_worker_env() {
    local target
    if [ "$(worker_runner)" = "orb" ]; then
        target="0.250.250.254:$(console_grpc_port)"
    else
        target="127.0.0.1:$(console_grpc_port)"
    fi
    upsert_env "$WORKER_ENV" WORKER_CONSOLE_GRPC_TARGET "$target"
    upsert_env "$WORKER_ENV" WORKER_TERMINAL_LEASE_MAX_SEC "$WORKER_LEASE_MAX_SEC"
    upsert_env "$WORKER_ENV" WORKER_PROXY_ENABLED true
    upsert_env "$WORKER_ENV" WORKER_PROXY_LISTEN_ADDR :8091
    upsert_env "$WORKER_ENV" WORKER_PROXY_ADVERTISE_ADDR "${DOCKER_GATEWAY}:8091"
}

provision_worker() {
    mkdir -p "$STATE_DIR"
    authenticate_console

    local node_id response secret
    node_id="$(env_value "$WORKER_ENV" WORKER_ID)"
    if [ -n "$node_id" ]; then
        if console_api "/api/v1/workers?status=all&page=1&page_size=100" |
            jq -e --arg node_id "$node_id" '.items[]? | select(.node_id == $node_id)' >/dev/null; then
            sync_worker_env
            echo "• worker-docker 凭据有效，继续复用"
            return
        fi
        echo "• 本机 Worker 凭据已不属于当前 Console，正在重新创建"
    fi

    response="$(console_api -X POST -H 'Content-Type: application/json' -d '{"type":"normal"}' /api/v1/workers)"
    node_id="$(printf '%s' "$response" | jq -r '.node_id // empty')"
    secret="$(printf '%s' "$response" | jq -r '.worker_secret // empty')"
    [ -n "$node_id" ] && [ -n "$secret" ] || die "Console 返回了无效的 Worker 凭据"
    write_worker_env "$node_id" "$secret"
    echo "• 已创建 worker-docker 凭据（scripts/.dev/worker-docker.env）"
}

build_worker_linux() {
    require_command go
    local docker_arch go_arch rebuild=0
    docker_arch="$(docker info --format '{{.Architecture}}')"
    case "$docker_arch" in
        aarch64 | arm64) go_arch="arm64" ;;
        x86_64 | amd64) go_arch="amd64" ;;
        *) die "不支持的 Docker 架构：$docker_arch" ;;
    esac
    if [ ! -x "$WORKER_BINARY" ]; then
        rebuild=1
    elif find "$ROOT_DIR/worker/worker-docker" "$ROOT_DIR/worker/internal" "$ROOT_DIR/worker/go.mod" "$ROOT_DIR/worker/go.sum" "$ROOT_DIR/api" -type f \( -name '*.go' -o -name 'go.mod' -o -name 'go.sum' \) -newer "$WORKER_BINARY" -print -quit | grep -q .; then
        rebuild=1
    fi
    if [ "$rebuild" -eq 1 ]; then
        echo "• 正在编译 Linux/$go_arch worker-docker"
        (cd "$ROOT_DIR/worker/worker-docker" && CGO_ENABLED=0 GOOS=linux GOARCH="$go_arch" go build -o "$WORKER_BINARY" ./cmd/worker-docker)
    fi
}

container_running() {
    [ "$(docker inspect -f '{{.State.Running}}' "$1" 2>/dev/null || true)" = "true" ]
}

start_worker_orb() {
    require_command orb
    require_docker
    ensure_network
    if container_running "$WORKER_CONTAINER"; then
        echo "• worker-docker 已在 OrbStack 中运行，跳过"
        return
    fi
    docker rm -f "$WORKER_CONTAINER" >/dev/null 2>&1 || true
    build_worker_linux
    docker run -d --name "$WORKER_CONTAINER" \
        --privileged --pid host --security-opt label=disable \
        --network "$DOCKER_NETWORK" \
        --env-file "$WORKER_ENV" \
        -v "$WORKER_BINARY:/worker-docker:ro" \
        -v /var/run/docker.sock:/var/run/docker.sock \
        "$WORKER_IMAGE" nsenter -t 1 -n /worker-docker >/dev/null
    echo "• worker-docker 已在 OrbStack Linux VM 中启动（端口 8091）"
}

stop_worker_orb() {
    require_docker
    if docker rm -f "$WORKER_CONTAINER" >/dev/null 2>&1; then
        echo "• worker-docker 已停止"
    else
        echo "• worker-docker 未在运行"
    fi
}

render_nginx_config() {
    local domain token domain_regex
    domain="$(dev_env_value CONSOLE_PROXY_PUBLIC_BASE_DOMAIN)"
    token="$(dev_env_value CONSOLE_PROXY_INTERNAL_AUTH_TOKEN)"
    [ -n "$domain" ] || die "scripts/dev.env 缺少 CONSOLE_PROXY_PUBLIC_BASE_DOMAIN"
    [ -n "$token" ] || die "scripts/dev.env 缺少 CONSOLE_PROXY_INTERNAL_AUTH_TOKEN"
    mkdir -p "$STATE_DIR"
    case "$domain" in *[!A-Za-z0-9.-]*) die "公共预览域名包含不支持的字符：$domain" ;; esac
    case "$token" in *$'\n'* | *'"'*) die "CONSOLE_PROXY_INTERNAL_AUTH_TOKEN 包含不支持的字符" ;; esac
    domain_regex="$(printf '%s' "$domain" | sed 's/\./\\./g')"
    sed \
        -e "s/__PUBLIC_PREVIEW_DOMAIN_REGEX__/$domain_regex/g" \
        -e "s/__PUBLIC_PREVIEW_DOMAIN__/$domain/g" \
        -e "s/__CONSOLE_HTTP_PORT__/$(console_http_port)/g" \
        -e "s/__CONSOLE_PROXY_INTERNAL_AUTH_TOKEN__/$token/g" \
        "$NGINX_TEMPLATE" >"$NGINX_CONFIG"
    chmod 600 "$NGINX_CONFIG"
}

start_nginx() {
    require_docker
    wait_console
    ensure_network
    render_nginx_config
    if container_running "$NGINX_CONTAINER"; then
        echo "• nginx 已在运行，跳过"
        return
    fi
    docker rm -f "$NGINX_CONTAINER" >/dev/null 2>&1 || true
    docker run -d --name "$NGINX_CONTAINER" \
        --network "$DOCKER_NETWORK" \
        --add-host host.docker.internal:host-gateway \
        -p "$HTTP_PORT:80" \
        -v "$NGINX_CONFIG:/etc/nginx/nginx.conf:ro" \
        "$NGINX_IMAGE" >/dev/null
    echo "• nginx 已启动（http://*.$(dev_env_value CONSOLE_PROXY_PUBLIC_BASE_DOMAIN):${HTTP_PORT}）"
}

stop_nginx() {
    require_docker
    if docker rm -f "$NGINX_CONTAINER" >/dev/null 2>&1; then
        echo "• nginx 已停止"
    else
        echo "• nginx 未在运行"
    fi
}

cleanup_network() {
    require_docker
    docker network rm "$DOCKER_NETWORK" >/dev/null 2>&1 || true
}

status_component() {
    require_docker
    local component="$1" container port state docker_status image
    case "$component" in
        worker-docker) container="$WORKER_CONTAINER"; port=8091 ;;
        nginx) container="$NGINX_CONTAINER"; port="$HTTP_PORT" ;;
        *) die "未知组件：$component" ;;
    esac
    docker_status="$(docker ps -a --filter "name=^/${container}$" --format '{{.Status}}' | head -n 1)"
    image="$(docker inspect -f '{{.Config.Image}}' "$container" 2>/dev/null || true)"
    if [ -n "$docker_status" ]; then
        state="${docker_status}${image:+（${image}）}"
    else
        state="未创建"
    fi
    printf '%-14s %-14s %-8s %s\n' "$component" "$port" "$(if lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then echo LISTEN; else echo -; fi)" "$state"
}

logs_component() {
    require_docker
    case "$1" in
        worker-docker) docker logs --tail 200 "$WORKER_CONTAINER" ;;
        nginx) docker logs --tail 200 "$NGINX_CONTAINER" ;;
        *) die "未知组件：$1" ;;
    esac
}

case "${1:-}" in
    runner) worker_runner ;;
    provision-worker) provision_worker ;;
    start-worker-orb) start_worker_orb ;;
    stop-worker-orb) stop_worker_orb ;;
    start-nginx) start_nginx ;;
    stop-nginx) stop_nginx ;;
    cleanup-network) cleanup_network ;;
    status) status_component "${2:-}" ;;
    logs) logs_component "${2:-}" ;;
    worker-env) printf '%s' "$WORKER_ENV" ;;
    port)
        case "${2:-}" in worker-docker) printf '8091' ;; nginx) printf '%s' "$HTTP_PORT" ;; *) exit 1 ;; esac
        ;;
    *) die "内部用法：dev-public-preview.sh <runner|provision-worker|start-worker-orb|stop-worker-orb|start-nginx|stop-nginx|status|logs|worker-env|port>" ;;
esac

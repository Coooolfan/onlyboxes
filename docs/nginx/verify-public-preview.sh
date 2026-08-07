#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/onlyboxes-nginx-verify.XXXXXX")"
suffix="$$"
network="onlyboxes-nginx-verify-${suffix}"
worker_container="onlyboxes-nginx-worker-${suffix}"
console_container="onlyboxes-nginx-console-${suffix}"
public_container="onlyboxes-nginx-public-${suffix}"
preview_host="ceirceirceirceirceirceirce.public-preview.example.com"

cleanup() {
    docker rm -f "$public_container" "$console_container" "$worker_container" >/dev/null 2>&1 || true
    docker network rm "$network" >/dev/null 2>&1 || true
    rm -rf "$tmp_dir"
}
trap cleanup EXIT

for command in docker curl openssl sed grep mktemp tr; do
    command -v "$command" >/dev/null || {
        echo "missing required command: $command" >&2
        exit 1
    }
done

docker network create "$network" >/dev/null

cat >"$tmp_dir/worker.conf" <<'EOF'
events {}
http {
    server {
        listen 8091;
        location / {
            add_header X-E2E-Route-Token "$http_x_onlyboxes_route_token" always;
            add_header X-E2E-Internal-Token "$http_x_onlyboxes_internal_token" always;
            add_header X-E2E-Original-Host "$http_x_original_host" always;
            add_header X-E2E-Upstream "$http_x_onlyboxes_upstream" always;
            add_header X-E2E-Authorization "$http_authorization" always;
            add_header X-E2E-Cookie "$http_cookie" always;
            add_header X-E2E-Host "$host" always;
            return 200 "preview-ok\n";
        }
    }
}
EOF

docker run -d --name "$worker_container" --network "$network" \
    -v "$tmp_dir/worker.conf:/etc/nginx/nginx.conf:ro" \
    nginx:1.26-alpine >/dev/null
worker_ip="$(docker inspect "$worker_container" --format "{{with index .NetworkSettings.Networks \"$network\"}}{{.IPAddress}}{{end}}")"

cat >"$tmp_dir/console.conf" <<EOF
events {}
http {
    server {
        listen 8089;
        location = /internal/v1/proxy/resolve {
            if (\$http_x_onlyboxes_internal_token != "replace-with-console-proxy-internal-token") { return 401; }
            if (\$http_x_original_host != "$preview_host") { return 403; }
            add_header X-Onlyboxes-Upstream "http://$worker_ip:8091" always;
            add_header X-Onlyboxes-Route-Token "route-token-from-console" always;
            return 204;
        }
    }
}
EOF

docker run -d --name "$console_container" --network "$network" \
    -v "$tmp_dir/console.conf:/etc/nginx/nginx.conf:ro" \
    nginx:1.26-alpine >/dev/null
console_ip="$(docker inspect "$console_container" --format "{{with index .NetworkSettings.Networks \"$network\"}}{{.IPAddress}}{{end}}")"

openssl req -x509 -nodes -newkey rsa:2048 -days 1 \
    -subj '/CN=*.public-preview.example.com' \
    -keyout "$tmp_dir/public-preview.key" \
    -out "$tmp_dir/public-preview.fullchain.pem" >/dev/null 2>&1
sed "s#127\\.0\\.0\\.1:8089#$console_ip:8089#" \
    "$root_dir/docs/nginx/public-preview.conf.example" >"$tmp_dir/public-preview.conf"
printf 'events {}\nhttp { include /etc/nginx/public-preview.conf; }\n' >"$tmp_dir/public.conf"

docker run -d --name "$public_container" --network "$network" \
    -p 127.0.0.1::443 \
    -v "$tmp_dir/public.conf:/etc/nginx/nginx.conf:ro" \
    -v "$tmp_dir/public-preview.conf:/etc/nginx/public-preview.conf:ro" \
    -v "$tmp_dir/public-preview.key:/etc/nginx/tls/public-preview.key:ro" \
    -v "$tmp_dir/public-preview.fullchain.pem:/etc/nginx/tls/public-preview.fullchain.pem:ro" \
    nginx:1.26-alpine >/dev/null
host_mapping="$(docker port "$public_container" 443/tcp)"
host_port="${host_mapping##*:}"
resolve_arg="$preview_host:$host_port:127.0.0.1"

anonymous_response=""
for _ in {1..50}; do
    if anonymous_response="$(curl --noproxy '*' -ksS -D - \
        --resolve "$resolve_arg" "https://$preview_host:$host_port/anonymous" 2>/dev/null | tr -d '\r')"; then
        break
    fi
    sleep 0.1
done
[[ -n "$anonymous_response" ]]
grep -Eq '^HTTP/(1\.1|2) 200[[:space:]]*$' <<<"$anonymous_response"
grep -q '^preview-ok$' <<<"$anonymous_response"
grep -q '^x-e2e-route-token: route-token-from-console$' <<<"$anonymous_response"
if grep -Eq '^x-e2e-(authorization|cookie):' <<<"$anonymous_response"; then
    echo "anonymous request unexpectedly carried application credentials" >&2
    exit 1
fi

header_response="$(curl --noproxy '*' -ksS -D - \
    --resolve "$resolve_arg" "https://$preview_host:$host_port/headers" \
    -H 'Authorization: Bearer app-token' \
    -H 'Cookie: app_session=abc' \
    -H 'X-Onlyboxes-Route-Token: client-forged' \
    -H 'X-Onlyboxes-Upstream: 127.0.0.1:22' \
    -H 'X-Onlyboxes-Internal-Token: client-forged' \
    -H 'X-Original-Host: attacker.example' | tr -d '\r')"
grep -Eq '^HTTP/(1\.1|2) 200[[:space:]]*$' <<<"$header_response"
grep -q '^x-e2e-route-token: route-token-from-console$' <<<"$header_response"
grep -q '^x-e2e-authorization: Bearer app-token$' <<<"$header_response"
grep -q '^x-e2e-cookie: app_session=abc$' <<<"$header_response"
grep -q "^x-e2e-host: $preview_host$" <<<"$header_response"
if grep -Eq '^x-e2e-(internal-token|original-host|upstream):' <<<"$header_response"; then
    echo "client-forged internal header reached the worker" >&2
    exit 1
fi

invalid_status="$(curl --noproxy '*' -ksS -o /dev/null -w '%{http_code}' \
    --resolve "invalid.public-preview.example.com:$host_port:127.0.0.1" \
    "https://invalid.public-preview.example.com:$host_port/")"
[[ "$invalid_status" == "403" ]]

resolver_requests="$(docker logs "$console_container" 2>&1 | grep -c 'GET /internal/v1/proxy/resolve')"
[[ "$resolver_requests" == "2" ]]

echo "public preview Nginx verification passed"

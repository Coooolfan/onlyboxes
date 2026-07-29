#!/usr/bin/env bash
# Onlyboxes 本地开发进程编排：用一个 tmux 会话托管 console / web / website，
# 避免 go run 与 vite dev 阻塞终端。日志同时落到 scripts/.dev/<svc>.log。
set -euo pipefail

SESSION="${ONLYBOXES_DEV_SESSION:-onlyboxes-dev}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
LOG_DIR="$SCRIPT_DIR/.dev"
CREDS_FILE="$LOG_DIR/console-creds.json"
# tmux server 不继承调用方的自定义环境变量，因此额外配置统一走这个文件
ENV_FILE="${ONLYBOXES_DEV_ENV:-$SCRIPT_DIR/dev.env}"

ALL_SERVICES=(console web website)

svc_dir() {
    case "$1" in
        console) echo "$ROOT_DIR/console" ;;
        web) echo "$ROOT_DIR/web" ;;
        website) echo "$ROOT_DIR/website" ;;
        *) return 1 ;;
    esac
}

svc_cmd() {
    case "$1" in
        # console 默认 JSON 日志，无需处理 ANSI；两个前端关闭彩色输出，避免日志混入转义序列
        console) echo "go run ./cmd/console" ;;
        web) echo "NO_COLOR=1 yarn dev" ;;
        website) echo "NO_COLOR=1 yarn dev" ;;
        *) return 1 ;;
    esac
}

# 主端口：用于 status 展示与 stop 后的兜底清理
svc_port() {
    case "$1" in
        console) echo 8089 ;;
        web) echo 5178 ;;
        website) echo 5173 ;;
        *) echo "" ;;
    esac
}

# 需要一并回收的附加端口（console 还监听 gRPC）
svc_extra_ports() {
    case "$1" in
        console) echo 50051 ;;
        *) echo "" ;;
    esac
}

# 服务命令包一层 tee：面板显示与日志落盘同源。
# 前置 source dev.env：tmux server 环境与调用方隔离，配置只能在窗口内加载。
wrapped_cmd() {
    echo "set -a; [ -f '$ENV_FILE' ] && . '$ENV_FILE'; set +a; $(svc_cmd "$1") 2>&1 | tee '$LOG_DIR/$1.log'"
}

die() {
    echo "错误：$*" >&2
    exit 1
}

require_tmux() {
    command -v tmux >/dev/null 2>&1 || die "未找到 tmux，请先安装（brew install tmux）"
}

is_valid_service() {
    local svc="$1"
    for s in "${ALL_SERVICES[@]}"; do
        [ "$svc" = "$s" ] && return 0
    done
    return 1
}

# 解析服务参数：为空则返回全部，否则逐个校验
resolve_services() {
    if [ "$#" -eq 0 ]; then
        printf '%s\n' "${ALL_SERVICES[@]}"
        return
    fi
    for svc in "$@"; do
        is_valid_service "$svc" || die "未知服务 '$svc'（可选：${ALL_SERVICES[*]}）"
        echo "$svc"
    done
}

session_exists() {
    tmux has-session -t "$SESSION" 2>/dev/null
}

window_exists() {
    tmux list-windows -t "$SESSION" -F '#{window_name}' 2>/dev/null | grep -qx "$1"
}

port_listening() {
    local port="$1"
    [ -n "$port" ] || return 1
    lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1
}

# 递归列出某进程及其所有子孙的 PID
descendant_pids() {
    local pid="$1" child
    [ -n "$pid" ] || return 0
    echo "$pid"
    for child in $(pgrep -P "$pid" 2>/dev/null || true); do
        descendant_pids "$child"
    done
}

# 窗口进程树快照：必须在 kill-window 之前采集
snapshot_pids() {
    local svc="$1" pane_pid
    pane_pid="$(tmux list-panes -t "$SESSION:$svc" -F '#{pane_pid}' 2>/dev/null | head -n 1 || true)"
    [ -n "$pane_pid" ] || return 0
    descendant_pids "$pane_pid"
}

# go run 会 fork 出真正的二进制，tmux 杀窗口后子进程可能残留并占住端口，
# 因此停止服务后做一次兜底回收。
#
# 只回收「既占用该端口、又属于本会话窗口进程树」的 PID：端口上的进程可能是
# 用户手动启动的服务，无差别按端口 kill 会误杀本脚本管辖之外的进程。
reclaim_ports() {
    local svc="$1" owned_pids="$2" port
    [ -n "$owned_pids" ] || return 0

    for port in $(svc_port "$svc") $(svc_extra_ports "$svc"); do
        [ -n "$port" ] || continue
        port_listening "$port" || continue

        local port_pids targets
        # 只取监听者：不加 -sTCP:LISTEN 会把连到该端口的客户端进程也算进来
        port_pids="$(lsof -ti tcp:"$port" -sTCP:LISTEN 2>/dev/null || true)"
        [ -n "$port_pids" ] || continue

        # 取交集：仅本窗口进程树内的 PID
        targets="$(comm -12 \
            <(printf '%s\n' "$port_pids" | sort -u) \
            <(printf '%s\n' "$owned_pids" | sort -u))"
        if [ -z "$targets" ]; then
            echo "• 端口 $port 被本脚本之外的进程占用，未做处理（lsof -i tcp:${port}）"
            continue
        fi

        # shellcheck disable=SC2086
        kill $targets 2>/dev/null || true
        for _ in 1 2 3 4 5 6 7 8 9 10; do
            # shellcheck disable=SC2086
            kill -0 $targets 2>/dev/null || break
            sleep 0.3
        done
        # shellcheck disable=SC2086
        kill -9 $targets 2>/dev/null || true

        echo "• 已回收 $svc 在端口 $port 的残留进程"
    done
}

ensure_session() {
    if session_exists; then
        return
    fi
    # 先建一个占位 window，待服务 window 建好后再移除
    tmux new-session -d -s "$SESSION" -n __bootstrap__ -c "$ROOT_DIR"
}

cleanup_bootstrap() {
    if window_exists __bootstrap__; then
        tmux kill-window -t "$SESSION:__bootstrap__" 2>/dev/null || true
    fi
}

start_service() {
    local svc="$1" dir log port
    dir="$(svc_dir "$svc")"
    log="$LOG_DIR/$svc.log"

    if window_exists "$svc"; then
        if tmux list-panes -t "$SESSION:$svc" -F '#{pane_dead}' 2>/dev/null | grep -qx 1; then
            # 进程已退出（窗口因 remain-on-exit 残留），原位复活
            : >"$log"
            tmux respawn-window -k -t "$SESSION:$svc" -c "$dir" "$(wrapped_cmd "$svc")"
            echo "• $svc 进程已退出，已在原窗口重启（端口 $(svc_port "$svc")，日志 scripts/.dev/$svc.log）"
        else
            echo "• $svc 已在运行，跳过"
        fi
        return
    fi

    port="$(svc_port "$svc")"
    if port_listening "$port"; then
        echo "• 警告：端口 $port 已被其他进程占用，$svc 可能启动失败" >&2
    fi

    mkdir -p "$LOG_DIR"
    : >"$log"

    # 服务命令直接作为窗口进程运行：进程退出即 pane 死亡，status 能准确反映状态。
    # 输出经 tee 落盘：stdout 非 TTY 时 vite 自动退化为纯文本行式输出，
    # 日志不会混入屏幕重绘/光标移动转义序列
    tmux new-window -t "$SESSION" -n "$svc" -c "$dir" "$(wrapped_cmd "$svc")"
    # 进程退出后保留 pane，便于 status 显示「已退出」并支持原位复活
    tmux set-window-option -t "$SESSION:$svc" remain-on-exit on
    echo "• ${svc} 启动中（端口 ${port}，日志 scripts/.dev/$svc.log）"
}

stop_service() {
    local svc="$1" owned
    if ! window_exists "$svc"; then
        echo "• $svc 未在运行"
        return
    fi
    # 进程树快照必须先于 kill-window 采集，否则无从判断端口上的进程是否归本脚本管
    owned="$(snapshot_pids "$svc")"
    tmux kill-window -t "$SESSION:$svc"
    echo "• $svc 已停止"
    reclaim_ports "$svc" "$owned"
}

cmd_start() {
    require_tmux
    local services
    services=$(resolve_services "$@")
    ensure_session
    while IFS= read -r svc; do
        start_service "$svc"
    done <<<"$services"
    cleanup_bootstrap
    echo
    echo "已就绪。状态：scripts/dev.sh status ｜ 日志：scripts/dev.sh logs <svc> ｜ 控制台账号：scripts/dev.sh creds"
}

cmd_stop() {
    require_tmux
    local services
    services=$(resolve_services "$@")

    if ! session_exists; then
        echo "会话 $SESSION 未运行"
        return
    fi

    if [ "$#" -eq 0 ]; then
        # 先逐个采集进程树快照，再销毁会话，最后按快照回收
        local snapshots="" svc_snapshot
        while IFS= read -r svc; do
            window_exists "$svc" || continue
            svc_snapshot="$(snapshot_pids "$svc" | tr '\n' ' ')"
            snapshots+="$svc|$svc_snapshot"$'\n'
        done <<<"$services"

        tmux kill-session -t "$SESSION"
        echo "已停止全部服务（会话 $SESSION 已销毁）"

        while IFS='|' read -r svc pids; do
            [ -n "$svc" ] || continue
            # shellcheck disable=SC2086
            reclaim_ports "$svc" "$(printf '%s\n' $pids)"
        done <<<"$snapshots"
        return
    fi

    while IFS= read -r svc; do
        stop_service "$svc"
    done <<<"$services"

    # 若只剩占位或空会话则一并销毁
    if ! tmux list-windows -t "$SESSION" -F '#{window_name}' 2>/dev/null | grep -qvx __bootstrap__; then
        tmux kill-session -t "$SESSION" 2>/dev/null || true
    fi
}

cmd_restart() {
    cmd_stop "$@"
    sleep 1
    cmd_start "$@"
}

cmd_status() {
    require_tmux
    if ! session_exists; then
        echo "会话 ${SESSION}：未运行"
        return
    fi
    echo "会话 ${SESSION}：运行中"
    # 表头含中文，显示宽度与 printf 的字符计数不一致，这里按显示宽度手工对齐
    echo "服务       端口           监听     窗口状态"
    for svc in "${ALL_SERVICES[@]}"; do
        local port extra ports listen win all_listening
        port="$(svc_port "$svc")"
        extra="$(svc_extra_ports "$svc")"
        ports="$port${extra:+/$extra}"

        all_listening=1
        for p in $port $extra; do
            port_listening "$p" || all_listening=0
        done

        if window_exists "$svc"; then
            if tmux list-panes -t "$SESSION:$svc" -F '#{pane_dead}' 2>/dev/null | grep -qx 1; then
                win="已退出"
            else
                win="运行中"
            fi
        else
            win="未启动"
        fi

        if [ "$all_listening" -eq 1 ]; then
            listen="LISTEN"
            # 本脚本没起这个服务，端口却在监听：多半是手动启动的实例，明确标注以免误读
            [ "$win" = "运行中" ] || win="${win}（端口被外部进程占用）"
        else
            listen="-"
        fi
        printf '%-10s %-14s %-8s %s\n' "$svc" "$ports" "$listen" "$win"
    done
}

cmd_logs() {
    local svc="${1:-}"
    [ -n "$svc" ] || die "用法：scripts/dev.sh logs <${ALL_SERVICES[*]}>"
    is_valid_service "$svc" || die "未知服务 '$svc'（可选：${ALL_SERVICES[*]}）"
    local log="$LOG_DIR/$svc.log"
    [ -f "$log" ] || die "暂无 ${svc} 日志（先执行 scripts/dev.sh start ${svc}）"
    tail -n 200 "$log"
}

# 从扁平 JSON 日志行里取一个字符串字段
json_field() {
    printf '%s' "$1" | sed -n "s/.*\"$2\":\"\([^\"]*\)\".*/\1/p"
}

print_creds() {
    local line="$1" username password api_key_name api_key
    username="$(json_field "$line" username)"
    password="$(json_field "$line" password)"
    api_key_name="$(json_field "$line" api_key_name)"
    api_key="$(json_field "$line" api_key)"

    echo "console 管理员账号（首次初始化时生成）"
    echo "  用户名：$username"
    echo "  密码：  $password"
    if [ -n "$api_key" ]; then
        echo "  API Key（${api_key_name}）：$api_key"
    fi
    echo
    echo "登录地址：http://localhost:5178"
}

# console 只在首次初始化时把管理员密码打进日志，而 start 每次会清空日志，
# 因此首次抓到后固化到 scripts/.dev/console-creds.json 长期保留。
cmd_creds() {
    if [ -f "$CREDS_FILE" ]; then
        print_creds "$(cat "$CREDS_FILE")"
        return
    fi

    local log="$LOG_DIR/console.log" line
    if [ -f "$log" ]; then
        line="$(grep -F 'console admin account initialized' "$log" | tail -n 1 || true)"
        if [ -n "$line" ]; then
            mkdir -p "$LOG_DIR"
            printf '%s\n' "$line" >"$CREDS_FILE"
            chmod 600 "$CREDS_FILE"
            print_creds "$line"
            return
        fi
    fi

    cat >&2 <<EOF
未找到控制台管理员凭据。

凭据只在 console 首次初始化数据库时打印一次，本次启动读取的是已存在的账号。
如需重置：
  scripts/dev.sh stop console
  rm console/db/onlyboxes-console.db
  scripts/dev.sh start console && scripts/dev.sh creds
EOF
    exit 1
}

usage() {
    cat <<'EOF'
Onlyboxes 本地开发进程编排（基于 tmux）

用法：scripts/dev.sh <命令> [服务...]

命令：
  start   [svc...]   后台启动服务（默认 console web website），终端不阻塞
  stop    [svc...]   停止指定服务；不带参数销毁整个会话
  restart [svc...]   重启指定服务
  status             查看会话、端口监听与各窗口状态
  logs    <svc>      查看某服务最近日志（快照，立即返回）
  creds              打印 console 管理员账号（首次初始化时生成）

说明：不提供 attach / logs -f 等阻塞入口；如需实时查看请自行执行 tmux attach -t $SESSION

服务：console（:8089 HTTP，:50051 gRPC）web（:5178）website（:5173）

不含 worker：worker 各实现启动参数差异较大，部分场景需同时运行多个实例，请手动启动。

示例：
  scripts/dev.sh start              # 三个全起
  scripts/dev.sh start console web  # 只起控制节点与前端
  scripts/dev.sh logs console       # 查看控制节点最近日志
  scripts/dev.sh restart web        # 重启前端
  scripts/dev.sh stop               # 全部停止

环境变量：
  ONLYBOXES_DEV_SESSION  自定义 tmux 会话名（默认 onlyboxes-dev）
  ONLYBOXES_DEV_ENV      自定义配置文件路径（默认 scripts/dev.env）

配置：tmux 不继承当前 shell 的自定义环境变量，服务所需配置请写入
      scripts/dev.env（参考 scripts/dev.env.example），start 时自动加载。
EOF
}

main() {
    local cmd="${1:-}"
    [ "$#" -gt 0 ] && shift || true
    case "$cmd" in
        start) cmd_start "$@" ;;
        stop) cmd_stop "$@" ;;
        restart) cmd_restart "$@" ;;
        status) cmd_status "$@" ;;
        logs) cmd_logs "$@" ;;
        creds) cmd_creds "$@" ;;
        "" | -h | --help | help) usage ;;
        *) die "未知命令 '$cmd'（执行 scripts/dev.sh --help 查看用法）" ;;
    esac
}

main "$@"

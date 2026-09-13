#!/usr/bin/env bash
#
# FRP-More 一键部署脚本
#
# 用法:
#   curl -fsSL https://raw.githubusercontent.com/JuZiool/frp-more/main/install.sh | bash
#
# 可用环境变量:
#   FRP_MORE_PORT     管理端口        (默认 1332)
#   FRP_MORE_DIR      数据目录        (默认 /opt/frp-more)
#   FRP_MORE_VERSION  镜像版本        (默认 latest，可指定 v0.1)
#   FRP_MORE_IMAGE    完整镜像地址    (默认 ghcr.io/juziool/frp-more:<版本>)
#
# 示例: 指定端口和版本
#   curl -fsSL https://raw.githubusercontent.com/JuZiool/frp-more/main/install.sh | \
#     FRP_MORE_PORT=1332 FRP_MORE_VERSION=v0.1 bash

set -euo pipefail

FRP_MORE_PORT="${FRP_MORE_PORT:-1332}"
FRP_MORE_DIR="${FRP_MORE_DIR:-/opt/frp-more}"
FRP_MORE_VERSION="${FRP_MORE_VERSION:-latest}"
FRP_MORE_IMAGE="${FRP_MORE_IMAGE:-ghcr.io/juziool/frp-more:${FRP_MORE_VERSION}}"
CONTAINER_NAME="frp-more"

log()  { printf '\033[32m[FRP-More]\033[0m %s\n' "$*"; }
warn() { printf '\033[33m[FRP-More]\033[0m %s\n' "$*"; }
die()  { printf '\033[31m[FRP-More]\033[0m %s\n' "$*" >&2; exit 1; }

# ---------- 环境检查 ----------
command -v docker >/dev/null 2>&1 \
  || die "未检测到 docker，请先安装 Docker: https://docs.docker.com/engine/install/"
docker info >/dev/null 2>&1 \
  || die "Docker 未运行，或当前用户无权访问（可尝试 sudo 运行本脚本）"

case "$(uname -m)" in
  x86_64|aarch64|arm64) ;;
  *) warn "当前架构 $(uname -m) 未经测试，镜像提供 amd64/arm64 版本，将继续尝试" ;;
esac

log "镜像: ${FRP_MORE_IMAGE}"
log "数据目录: ${FRP_MORE_DIR}"
log "管理端口: ${FRP_MORE_PORT}"

# ---------- 初始化配置目录 ----------
mkdir -p "${FRP_MORE_DIR}/instances"
if [ ! -f "${FRP_MORE_DIR}/instances/example.toml.example" ]; then
  cat > "${FRP_MORE_DIR}/instances/example.toml.example" <<'EOF'
# FRP 实例配置模板（TOML 格式）
#
# 使用方法（二选一）:
#   1. 复制本文件并改名为 <实例名>.toml，修改后打开管理页面点「扫描目录」
#   2. 直接在管理页面「新建实例」中在线创建
#
# 注意: 目录中以 .toml 结尾的文件都会被当作实例配置自动加载

serverAddr = "your-frps-host"   # frps 服务端地址
serverPort = 7000               # frps 服务端端口
loginFailExit = false           # 连接失败保持重试（推荐保留）

[[proxies]]
name = "web"
type = "tcp"
localIP = "127.0.0.1"
localPort = 8080
remotePort = 6001
EOF
  log "已创建配置模板: ${FRP_MORE_DIR}/instances/example.toml.example"
fi

# ---------- 拉取镜像 ----------
log "拉取镜像..."
docker pull "${FRP_MORE_IMAGE}"

# ---------- 启动容器（host 网络） ----------
log "启动容器..."
docker rm -f "${CONTAINER_NAME}" >/dev/null 2>&1 || true
docker run -d \
  --name "${CONTAINER_NAME}" \
  --restart unless-stopped \
  --network host \
  -e TZ="${TZ:-Asia/Shanghai}" \
  -v "${FRP_MORE_DIR}:/data" \
  "${FRP_MORE_IMAGE}" \
  -data /data -addr ":${FRP_MORE_PORT}" >/dev/null

sleep 2
if [ "$(docker inspect -f '{{.State.Running}}' "${CONTAINER_NAME}" 2>/dev/null)" != "true" ]; then
  docker logs "${CONTAINER_NAME}" 2>&1 | tail -20
  die "容器启动失败，请查看上方日志"
fi

SERVER_IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
[ -n "${SERVER_IP}" ] || SERVER_IP="<本机IP>"

log "部署完成!"
cat <<EOF

  管理页面 : http://${SERVER_IP}:${FRP_MORE_PORT}
  数据目录 : ${FRP_MORE_DIR}（实例配置: instances/*.toml）
  查看日志 : docker logs -f ${CONTAINER_NAME}
  停止服务 : docker rm -f ${CONTAINER_NAME}

  下一步: 打开管理页面「新建实例」，或将 *.toml 配置放入
          ${FRP_MORE_DIR}/instances/ 后点「扫描目录」

  更新版本 : 重新运行本脚本即可（FRP_MORE_VERSION=指定版本），数据不会丢失
EOF

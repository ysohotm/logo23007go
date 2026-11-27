#!/usr/bin/env bash
# sg99 上运行 logo23107（host 网络 + 东八区 + 持久化 app_data）
# Mongo 用本机 mongodb19011（127.0.0.1:19011，勿连 v99）；首次需同步 fonts/images
#
# 用法:
#   bash deploy/run-sg99.sh                 # latest
#   bash deploy/run-sg99.sh 2026.1126.1010  # 指定 tag
set -euo pipefail

IMAGE_TAG="${1:-latest}"
IMAGE="ghcr.io/ysohotm/logo23007/logo23007go:${IMAGE_TAG}"
NAME="${CONTAINER_NAME:-logo23107}"
HOST_DATA="${HOST_DATA:-/data/logomaker23007}"
PORT="${PORT:-23107}"
HOST="${SG99_HOST:-sg99}"

# 本机 Mongo（host 网络，仅听 127.0.0.1:19011）
DEFAULT_DBCONN='mongodb://127.0.0.1:19011'
DBCONN="${DBCONN:-$DEFAULT_DBCONN}"

ssh "$HOST" "mkdir -p \
  '$HOST_DATA/fonts' \
  '$HOST_DATA/images' \
  '$HOST_DATA/cache/covers' \
  '$HOST_DATA/cache/bg'
  timedatectl set-timezone Asia/Shanghai || true
"

# Docker
ssh "$HOST" 'export DEBIAN_FRONTEND=noninteractive
  if ! command -v docker >/dev/null 2>&1; then
    apt-get update -qq
    apt-get install -y -qq ca-certificates curl
    curl -fsSL https://get.docker.com | sh
  fi
  systemctl enable --now docker
'

# GHCR 登录态（从本机 / v99 复制，勿打印）
if [[ -f /root/.docker/config.json ]]; then
  ssh "$HOST" 'mkdir -p /root/.docker'
  scp -q /root/.docker/config.json "$HOST:/root/.docker/config.json"
elif ssh -o BatchMode=yes root@104.238.220.50 'test -f /root/.docker/config.json' 2>/dev/null; then
  ssh "$HOST" 'mkdir -p /root/.docker'
  scp -q -o ProxyJump=none root@104.238.220.50:/root/.docker/config.json /tmp/docker-config.sg99.json
  scp -q /tmp/docker-config.sg99.json "$HOST:/root/.docker/config.json"
  rm -f /tmp/docker-config.sg99.json
fi

# 字体/背景：本机 SSH 管道中转 v99 → sg99（两机互不通；images 约 3G，可 SKIP_IMAGES=1）
V99_HOST="${V99_HOST:-root@104.238.220.50}"
if [[ "${SKIP_SYNC:-0}" != "1" ]]; then
  echo "==> sync fonts: v99 → $HOST"
  ssh "$V99_HOST" "tar cf - -C /data/logomaker23007 fonts" \
    | ssh "$HOST" "mkdir -p '$HOST_DATA' && tar xf - -C '$HOST_DATA'"
  if [[ "${SKIP_IMAGES:-0}" != "1" ]]; then
    echo "==> sync images: v99 → $HOST（约 3G）"
    ssh "$V99_HOST" "tar cf - -C /data/logomaker23007 images" \
      | ssh "$HOST" "mkdir -p '$HOST_DATA' && tar xf - -C '$HOST_DATA'"
  fi
fi

ssh "$HOST" "set -euo pipefail
  IMAGE='$IMAGE'
  NAME='$NAME'
  HOST_DATA='$HOST_DATA'
  PORT='$PORT'
  DBCONN='$DBCONN'
  ROLLBACK_TAG=\"rollback-\$(TZ=Asia/Shanghai date +%Y%m%d)\"

  docker pull \"\$IMAGE\"
  if docker inspect \"\$NAME\" >/dev/null 2>&1; then
    OLD_IMAGE=\$(docker inspect \"\$NAME\" --format '{{.Config.Image}}')
    docker tag \"\$OLD_IMAGE\" \"ghcr.io/ysohotm/logo23007/logo23007go:\${ROLLBACK_TAG}\" || true
    echo \"已打回退 tag: \${ROLLBACK_TAG}\"
  fi
  docker stop \"\$NAME\" >/dev/null 2>&1 || true
  docker rm \"\$NAME\" >/dev/null 2>&1 || true
  docker run -d --name \"\$NAME\" --network host --restart always \\
    -e DBCONN=\"\$DBCONN\" \\
    -e DBNAME=\"\${DBNAME:-logo23107}\" \\
    -e TZ=Asia/Shanghai \\
    -e GOMEMLIMIT=\"\${GOMEMLIMIT:-3GiB}\" \\
    -e IMAGE_CONCURRENCY=\"\${IMAGE_CONCURRENCY:-8}\" \\
    -e IMAGE_WAIT=\"\${IMAGE_WAIT:-200ms}\" \\
    -e PORT=\"\$PORT\" \\
    -v /usr/share/zoneinfo/Asia/Shanghai:/etc/localtime:ro \\
    -v \"\$HOST_DATA:/app/app_data\" \\
    \"\$IMAGE\"
  sleep 2
  curl -fsS \"http://127.0.0.1:\${PORT}/api/health\"
  echo
  docker exec \"\$NAME\" date
  docker exec \"\$NAME\" printenv TZ
  docker logs \"\$NAME\" 2>&1 | tail -n 20
"

echo "完成: $HOST logo23107 @ :$PORT"

#!/usr/bin/env bash
# v99 上运行 / 滚动更新 logo23107（host 网络 + 东八区 + 持久化 app_data）
#
# 用法:
#   bash deploy/run-v99.sh                 # 用 latest
#   bash deploy/run-v99.sh 2026.1126.1010  # 指定 tag
#
# 缓存目录随 app_data 一起挂载，无需再单独挂 covers/bg：
#   宿主机 /data/logomaker23007/cache/covers  ↔ 容器 /app/app_data/cache/covers
#   宿主机 /data/logomaker23007/cache/bg      ↔ 容器 /app/app_data/cache/bg

set -euo pipefail

IMAGE_TAG="${1:-latest}"
IMAGE="ghcr.io/ysohotm/logo23007/logo23007go:${IMAGE_TAG}"
NAME="${CONTAINER_NAME:-logo23107}"
HOST_DATA="${HOST_DATA:-/data/logomaker23007}"
PORT="${PORT:-23107}"

# 预建持久化目录（含缓存），避免空挂载缺 fonts/images 时启动失败
mkdir -p \
  "$HOST_DATA/fonts" \
  "$HOST_DATA/images" \
  "$HOST_DATA/cache/covers" \
  "$HOST_DATA/cache/bg"

DBCONN_FILE="${DBCONN_FILE:-$HOST_DATA/.dbconn}"

if [[ -z "${DBCONN:-}" ]]; then
  if docker inspect "$NAME" >/dev/null 2>&1; then
    DBCONN="$(docker inspect "$NAME" --format '{{range .Config.Env}}{{println .}}{{end}}' | awk -F= '/^DBCONN=/{sub(/^DBCONN=/,""); print}')"
  fi
fi
if [[ -z "${DBCONN:-}" && -f "$DBCONN_FILE" ]]; then
  # shellcheck disable=SC1090
  DBCONN="$(<"$DBCONN_FILE")"
fi
if [[ -z "${DBCONN:-}" ]]; then
  echo "请设置 DBCONN，或先有一个可继承环境的旧容器 $NAME，或写入 $DBCONN_FILE" >&2
  exit 1
fi
# Persist so the next deploy still works after the container is removed.
umask 077
printf '%s\n' "$DBCONN" >"$DBCONN_FILE"
chmod 600 "$DBCONN_FILE" 2>/dev/null || true

ROLLBACK_TAG="rollback-$(TZ=Asia/Shanghai date +%Y%m%d)"
if docker image inspect "$IMAGE" >/dev/null 2>&1; then
  if docker inspect "$NAME" >/dev/null 2>&1; then
    OLD_IMAGE="$(docker inspect "$NAME" --format '{{.Config.Image}}')"
    docker tag "$OLD_IMAGE" "ghcr.io/ysohotm/logo23007/logo23007go:${ROLLBACK_TAG}" || true
    echo "已打回退 tag: ${ROLLBACK_TAG} <- ${OLD_IMAGE}"
  fi
fi

docker pull "$IMAGE" || true
docker stop "$NAME" >/dev/null 2>&1 || true
docker rm "$NAME" >/dev/null 2>&1 || true

docker run -d --name "$NAME" --network host --restart always \
  -e DBCONN="$DBCONN" \
  -e DBNAME="${DBNAME:-logo23107}" \
  -e TZ=Asia/Shanghai \
  -e GOMEMLIMIT="${GOMEMLIMIT:-3GiB}" \
  -e IMAGE_CONCURRENCY="${IMAGE_CONCURRENCY:-192}" \
  -e IMAGE_WAIT="${IMAGE_WAIT:-5s}" \
  -e PORT="$PORT" \
  -v /usr/share/zoneinfo/Asia/Shanghai:/etc/localtime:ro \
  -v "$HOST_DATA:/app/app_data" \
  "$IMAGE"

sleep 2
curl -fsS "http://127.0.0.1:${PORT}/api/health"
echo
docker exec "$NAME" date
docker exec "$NAME" printenv TZ
docker logs "$NAME" 2>&1 | tail -n 20
echo "缓存目录: $HOST_DATA/cache/{covers,bg}"

#!/usr/bin/env bash
#
# 手动构建并推送 Docker 镜像（Go 版）→ 默认发布到 GitHub Container Registry
# 用法:
#   ./pubdocker.sh              # 推送到 GHCR（默认）
#   ./pubdocker.sh ghcr         # 同上
#   ./pubdocker.sh hub          # 仅 Docker Hub
#   ./pubdocker.sh ghcr hub     # 同时推送到两个仓库
#
# 环境变量（可选）:
#   GHCR_NAMESPACE    GHCR 命名空间（默认 ysohotm）
#   GHCR_IMAGE_NAME   镜像名（默认 logo23007/logo23007go）
#   GHCR_USERNAME     登录用户名（默认与 GHCR_NAMESPACE 相同）
#   GHCR_TOKEN        GHCR token；未设置时自动用 `gh auth token`
#   DOCKER_USERNAME   Docker Hub 用户名（默认 ysoho）
#   IMAGE_NAME        Docker Hub 镜像名（默认 logo23007go）
#   DOCKER_TOKEN      Docker Hub token
#   VERSION           版本标签（默认 YYYY.MMdd.HHMM）
#   PLATFORM          构建平台（默认 linux/amd64）
#   NO_CLEANUP=1      推送后不删除本地镜像

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

DOCKER_USERNAME="${DOCKER_USERNAME:-ysoho}"
GHCR_NAMESPACE="${GHCR_NAMESPACE:-ysohotm}"
HUB_IMAGE_NAME="${IMAGE_NAME:-logo23007go}"
GHCR_IMAGE_NAME="${GHCR_IMAGE_NAME:-logo23007/logo23007go}"
VERSION="${VERSION:-$(date +"%Y.%m%d.%H%M")}"
PLATFORM="${PLATFORM:-linux/amd64}"

TARGETS=("${@:-ghcr}")

hub_ref() {
  echo "${DOCKER_USERNAME}/${HUB_IMAGE_NAME}"
}

ghcr_ref() {
  echo "ghcr.io/${GHCR_NAMESPACE}/${GHCR_IMAGE_NAME}"
}

login_hub() {
  if [[ -n "${DOCKER_TOKEN:-}" ]]; then
    echo "$DOCKER_TOKEN" | docker login -u "$DOCKER_USERNAME" --password-stdin
  else
    docker login
  fi
}

resolve_ghcr_token() {
  if [[ -n "${GHCR_TOKEN:-}" ]]; then
    echo "$GHCR_TOKEN"
    return 0
  fi
  if command -v gh >/dev/null 2>&1; then
    gh auth token 2>/dev/null && return 0
  fi
  return 1
}

login_ghcr() {
  local user="${GHCR_USERNAME:-$GHCR_NAMESPACE}"
  local token
  if token="$(resolve_ghcr_token)"; then
    echo "$token" | docker login ghcr.io -u "$user" --password-stdin
  else
    echo "未找到 GHCR_TOKEN，且 gh auth token 不可用，改为交互登录..." >&2
    docker login ghcr.io
  fi
}

build_image() {
  local primary_ref="$1"
  echo "正在构建镜像 (${PLATFORM})..."
  docker build \
    --platform "$PLATFORM" \
    -t "${primary_ref}:${VERSION}" \
    -t "${primary_ref}:latest" \
    .
}

push_image() {
  local ref="$1"
  local registry_name="$2"

  echo "正在登录 ${registry_name}..."
  case "$registry_name" in
    hub) login_hub ;;
    ghcr) login_ghcr ;;
  esac

  echo "正在推送到 ${registry_name}..."
  docker push "${ref}:${VERSION}"
  docker push "${ref}:latest"

  echo "  - ${ref}:${VERSION}"
  echo "  - ${ref}:latest"
}

cleanup_local() {
  local ref="$1"
  echo "删除本地镜像: ${ref}"
  docker rmi "${ref}:${VERSION}" 2>/dev/null || true
  docker rmi "${ref}:latest" 2>/dev/null || true
}

echo "========================================"
echo " logo23007go Docker 发布"
echo " 版本: ${VERSION}"
echo " 平台: ${PLATFORM}"
echo " 目标: ${TARGETS[*]}"
echo "========================================"

PRIMARY_REF=""
PUSHED_REFS=()

for target in "${TARGETS[@]}"; do
  case "$target" in
    hub)
      ref="$(hub_ref)"
      [[ -z "$PRIMARY_REF" ]] && PRIMARY_REF="$ref"
      if [[ ${#PUSHED_REFS[@]} -eq 0 ]]; then
        build_image "$ref"
      else
        docker tag "${PUSHED_REFS[0]}:${VERSION}" "${ref}:${VERSION}"
        docker tag "${PUSHED_REFS[0]}:latest" "${ref}:latest"
      fi
      push_image "$ref" "hub"
      PUSHED_REFS+=("$ref")
      ;;
    ghcr)
      ref="$(ghcr_ref)"
      [[ -z "$PRIMARY_REF" ]] && PRIMARY_REF="$ref"
      if [[ ${#PUSHED_REFS[@]} -eq 0 ]]; then
        build_image "$ref"
      else
        docker tag "${PUSHED_REFS[0]}:${VERSION}" "${ref}:${VERSION}"
        docker tag "${PUSHED_REFS[0]}:latest" "${ref}:latest"
      fi
      push_image "$ref" "ghcr"
      PUSHED_REFS+=("$ref")
      ;;
    *)
      echo "未知目标: $target（支持 ghcr / hub）" >&2
      exit 1
      ;;
  esac
done

echo ""
echo "镜像发布完成！"
for ref in "${PUSHED_REFS[@]}"; do
  echo "  ${ref}:${VERSION}"
  echo "  ${ref}:latest"
done

if [[ "${NO_CLEANUP:-0}" != "1" ]]; then
  for ref in "${PUSHED_REFS[@]}"; do
    cleanup_local "$ref"
  done
fi

echo "完成时间: $(date +"%Y-%m-%d %H:%M:%S")"

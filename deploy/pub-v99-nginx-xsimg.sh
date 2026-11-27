#!/usr/bin/env bash
# 把通用图床 nginx 配置发布到 v99 并 reload
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
HOST="${HOST:-root@104.238.220.50}"
LIMIT_SRC="$ROOT/deploy/nginx/00-limits.conf"
WILDCARD_SRC="$ROOT/deploy/nginx/00-xsimg-wildcard-v99.conf"

[[ -f "$LIMIT_SRC" && -f "$WILDCARD_SRC" ]] || { echo "missing nginx sources" >&2; exit 1; }

ssh "$HOST" 'mkdir -p /etc/nginx/conf.d /var/log/nginx
  ts=$(TZ=Asia/Shanghai date +%Y%m%d%H%M%S)
  for f in /etc/nginx/conf.d/00-img-limits.conf /etc/nginx/conf.d/00-xsimg-wildcard.conf; do
    [[ -f "$f" ]] && cp -a "$f" "${f}.bak.${ts}" || true
  done
'

scp -q "$LIMIT_SRC" "$HOST:/etc/nginx/conf.d/00-img-limits.conf"
scp -q "$WILDCARD_SRC" "$HOST:/etc/nginx/conf.d/00-xsimg-wildcard.conf"

ssh "$HOST" 'nginx -t && systemctl reload nginx && echo RELOAD_OK
  curl -fsS -o /dev/null -w "health_host:%{http_code}\n" -H "Host: xsimg.example.test" http://127.0.0.1/health
  curl -fsS -o /dev/null -w "vod:%{http_code}\n" -H "Host: xsimg.5200tv.org" "http://127.0.0.1/vod/nginx自检_300x400.jpg" || true
'
echo "done: $HOST xsimg wildcard"

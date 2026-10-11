#!/usr/bin/env bash
set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FRONTEND_DIST="$PROJECT_ROOT/frontend/dist"
BACKEND_DIST="$PROJECT_ROOT/internal/webui/dist"

if [[ ! -f "$FRONTEND_DIST/index.html" ]]; then
  echo "没有找到前端构建产物，请先执行 npm run build。" >&2
  exit 1
fi

mkdir -p "$BACKEND_DIST"
find "$BACKEND_DIST" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
cp -R "$FRONTEND_DIST"/. "$BACKEND_DIST"/

echo "前端资源已同步到 Go 内嵌目录。"

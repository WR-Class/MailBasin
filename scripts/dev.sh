#!/usr/bin/env bash
set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONFIG_PATH="$PROJECT_ROOT/config.json"

if [[ ! -f "$CONFIG_PATH" ]]; then
  CONFIG_PATH="$PROJECT_ROOT/config.example.json"
  echo "未找到 config.json，本次使用 config.example.json。"
fi

cleanup() {
  if [[ -n "${BACKEND_PID:-}" ]]; then
    kill "$BACKEND_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT INT TERM

echo "后端地址：http://127.0.0.1:8788"
(
  cd "$PROJECT_ROOT"
  go run . -config "$CONFIG_PATH"
) &
BACKEND_PID=$!

echo "前端地址：http://127.0.0.1:5174"
cd "$PROJECT_ROOT/frontend"
npm run dev

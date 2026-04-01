#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

POSTGRES_DSN="${POSTGRES_DSN:-postgres://areyouai:areyouai@localhost:5432/areyouai?sslmode=disable}"
API_ADDR="${API_ADDR:-127.0.0.1:8080}"
WEB_PORT="${WEB_PORT:-3000}"
API_BASE_URL="${NEXT_PUBLIC_API_BASE_URL:-http://127.0.0.1:8080}"
CLOSED_ROOM_GRACE_DELAY_SECONDS="${CLOSED_ROOM_GRACE_DELAY_SECONDS:-900}"

cleanup() {
  echo
  echo "stopping backend/frontend..."
  jobs -p | xargs -r kill 2>/dev/null || true
}
trap cleanup EXIT INT TERM

echo "[1/5] starting postgres + redis"
docker compose up -d postgres redis

echo "[2/5] waiting for postgres"
for i in $(seq 1 30); do
  if docker compose exec -T postgres pg_isready -U areyouai -d areyouai >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

echo "[3/5] applying migrations"
POSTGRES_DSN="$POSTGRES_DSN" go run ./cmd/migrate -action up

if [ ! -d "$ROOT_DIR/apps/web/node_modules" ]; then
  echo "[4/5] installing frontend dependencies"
  (cd "$ROOT_DIR/apps/web" && npm install)
else
  echo "[4/5] frontend dependencies already installed"
fi

echo "[5/5] starting backend + frontend"
echo "backend:  http://localhost:8080"
echo "frontend: http://localhost:${WEB_PORT}"

API_LOG="${ROOT_DIR}/.tmp_api.log"
POSTGRES_DSN="$POSTGRES_DSN" API_ADDR="$API_ADDR" CLOSED_ROOM_GRACE_DELAY_SECONDS="$CLOSED_ROOM_GRACE_DELAY_SECONDS" go run ./cmd/api >"$API_LOG" 2>&1 &
API_PID=$!

echo "waiting for backend health..."
for i in $(seq 1 30); do
  if curl -fsS "http://localhost:8080/healthz" >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "$API_PID" >/dev/null 2>&1; then
    echo "backend failed to start. recent log:"
    tail -n 80 "$API_LOG" || true
    exit 1
  fi
  sleep 1
done

if ! curl -fsS "http://localhost:8080/healthz" >/dev/null 2>&1; then
  echo "backend did not become healthy in time. recent log:"
  tail -n 80 "$API_LOG" || true
  exit 1
fi

echo "backend healthy"
(cd "$ROOT_DIR/apps/web" && NEXT_PUBLIC_API_BASE_URL="$API_BASE_URL" npm run dev -- -H 127.0.0.1 -p "$WEB_PORT") &

wait

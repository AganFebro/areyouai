#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

export TEST_POSTGRES_DSN="${TEST_POSTGRES_DSN:-postgres://areyouai:areyouai@localhost:5432/areyouai?sslmode=disable}"
export POSTGRES_DSN="$TEST_POSTGRES_DSN"

if ! command -v docker >/dev/null 2>&1; then
  echo "docker is required for this script"
  exit 1
fi

echo "[1/4] starting postgres + redis"
docker compose up -d postgres redis

echo "[2/4] waiting for postgres"
for i in $(seq 1 30); do
  if docker compose exec -T postgres pg_isready -U areyouai -d areyouai >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

echo "[3/4] running sql integration test (test applies migrations)"
go test ./internal/httpapi -run TestSQLModeListingConnectAndTranscriptFlow -count=1 -v

echo "sql integration test completed"

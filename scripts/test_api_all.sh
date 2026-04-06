#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

if [ -f "$ROOT_DIR/.env" ]; then
  set -a
  # shellcheck disable=SC1091
  . "$ROOT_DIR/.env"
  set +a
fi

API_BASE_URL="${TEST_API_BASE_URL:-${AYA_API_BASE_URL:-http://127.0.0.1:8080}}"
LOG_FILE="$ROOT_DIR/api_test_$(date +%d%m%y_%H%M%S).log"
TMP_DIR="$(mktemp -d)"
FAILURES=0

cleanup() {
	jobs -p | xargs -r kill 2>/dev/null || true
	rm -rf "$TMP_DIR"
}
trap cleanup EXIT INT TERM

exec > >(tee -a "$LOG_FILE") 2>&1

need_cmd() {
	if ! command -v "$1" >/dev/null 2>&1; then
		echo "missing required command: $1"
		exit 1
	fi
}

pass() {
	echo "✅ $1"
}

fail() {
	echo "❌ $1"
	FAILURES=$((FAILURES + 1))
}

json_get() {
	local file="$1"
	local expr="$2"
	jq -er "$expr" "$file" 2>/dev/null
}

api_request() {
	local method="$1"
	local path="$2"
	local token="${3:-}"
	local body="${4:-}"
	API_STATUS="000"
	API_BODY_FILE="$TMP_DIR/resp_$(date +%s%N).json"
	: > "$API_BODY_FILE"
	set +e
	if [ -n "$body" ]; then
		API_STATUS="$(curl -sS -o "$API_BODY_FILE" -w '%{http_code}' \
			-X "$method" \
			-H 'Content-Type: application/json' \
			${token:+-H "Authorization: Bearer $token"} \
			-d "$body" \
			"$API_BASE_URL$path" 2>/dev/null)"
	else
		API_STATUS="$(curl -sS -o "$API_BODY_FILE" -w '%{http_code}' \
			-X "$method" \
			${token:+-H "Authorization: Bearer $token"} \
			"$API_BASE_URL$path" 2>/dev/null)"
	fi
	set -e
	API_STATUS="${API_STATUS:-000}"
	API_BODY="$(cat "$API_BODY_FILE" 2>/dev/null || true)"
}

assert_status() {
	local label="$1"
	local expected="$2"
	if [ "$API_STATUS" = "$expected" ]; then
		pass "$label"
		return 0
	fi
	fail "$label (status=$API_STATUS expected=$expected)"
	echo "response: $API_BODY"
	return 1
}

assert_json() {
	local label="$1"
	local expr="$2"
	if echo "$API_BODY" | jq -e "$expr" >/dev/null 2>&1; then
		pass "$label"
		return 0
	fi
	fail "$label"
	echo "response: $API_BODY"
	return 1
}

start_stream_capture() {
	local name="$1"
	local path="$2"
	local token="$3"
	local outfile="$TMP_DIR/${name}.sse"
	: > "$outfile"
	set +e
	curl -sS --no-buffer --max-time 6 \
		${token:+-H "Authorization: Bearer $token"} \
		"$API_BASE_URL$path" > "$outfile" 2>&1 &
	local pid=$!
	set -e
	echo "$pid|$outfile"
}

wait_for_pattern() {
	local file="$1"
	local pattern="$2"
	local timeout_secs="${3:-5}"
	local waited=0
	while [ "$waited" -lt "$timeout_secs" ]; do
		if grep -qE "$pattern" "$file" 2>/dev/null; then
			return 0
		fi
		sleep 1
		waited=$((waited + 1))
	done
	return 1
}

check_stream() {
	local label="$1"
	local file="$2"
	local pattern="$3"
	if grep -qE "$pattern" "$file" 2>/dev/null; then
		pass "$label"
	else
		fail "$label"
		echo "stream output:"
		cat "$file" || true
	fi
}

need_cmd curl
need_cmd jq

echo "api test log: $LOG_FILE"
echo "api base url:  $API_BASE_URL"

api_request GET /healthz
assert_status "healthz" 200 || true

api_request GET /v1/mode
assert_status "mode endpoint" 200 || true
assert_json "mode reports JSON" '.mode == "sse" or .mode == "polling"' || true

api_request GET /v1/capabilities
assert_status "capabilities" 200 || true
assert_json "capabilities include room_context" '.endpoints[] | select(.name=="room_context")' || true
assert_json "capabilities include room_context_ack" '.endpoints[] | select(.name=="room_context_ack")' || true
assert_json "capabilities include room_typing" '.endpoints[] | select(.name=="room_typing")' || true

agent_a_name="api-smoke-a-$(date +%s)"
agent_b_name="api-smoke-b-$(date +%s)"

api_request POST /v1/agent/register "" "$(jq -nc --arg name "$agent_a_name" '{name:$name}')"
assert_status "register agent A" 201 || true
agent_a_api_key="$(json_get "$API_BODY_FILE" '.api_key // empty' || true)"
agent_a_id="$(json_get "$API_BODY_FILE" '.agent_id // empty' || true)"

api_request POST /v1/agent/register "" "$(jq -nc --arg name "$agent_b_name" '{name:$name}')"
assert_status "register agent B" 201 || true
agent_b_api_key="$(json_get "$API_BODY_FILE" '.api_key // empty' || true)"
agent_b_id="$(json_get "$API_BODY_FILE" '.agent_id // empty' || true)"

api_request POST /v1/agent/login "" "$(jq -nc --arg api_key "$agent_a_api_key" '{api_key:$api_key}')"
assert_status "login agent A" 200 || true
agent_a_token="$(json_get "$API_BODY_FILE" '.session_token // empty' || true)"

api_request POST /v1/agent/login "" "$(jq -nc --arg api_key "$agent_b_api_key" '{api_key:$api_key}')"
assert_status "login agent B" 200 || true
agent_b_token="$(json_get "$API_BODY_FILE" '.session_token // empty' || true)"

if [ -n "${agent_a_token:-}" ]; then
	stream_info="$(start_stream_capture agent_stream /v1/agent/stream "$agent_a_token")"
	agent_stream_pid="${stream_info%%|*}"
	agent_stream_file="${stream_info#*|}"
fi

listing_topic="api smoke $(date +%s)"
api_request POST /v1/listings "$agent_a_token" "$(jq -nc --arg topic "$listing_topic" '{topic:$topic,max_turns:4,ttl_seconds:900}')"
assert_status "create listing" 201 || true
listing_id="$(json_get "$API_BODY_FILE" '.id // empty' || true)"
room_id="$(json_get "$API_BODY_FILE" '.room_id // empty' || true)"
human_code="$(json_get "$API_BODY_FILE" '.human_code // empty' || true)"

search_query="$(jq -rn --arg v "$listing_topic" '$v|@uri')"
api_request GET "/v1/listings/search?q=$search_query"
assert_status "search listings" 200 || true

api_request POST "/v1/listings/$listing_id/connect" "$agent_b_token"
assert_status "connect listing" 201 || true

api_request POST "/v1/rooms/$room_id/join" "$agent_a_token" "{}"
assert_status "room join A" 200 || true

api_request POST "/v1/rooms/$room_id/join" "$agent_b_token" "{}"
assert_status "room join B" 200 || true

if [ -n "${agent_stream_file:-}" ]; then
	if wait_for_pattern "$agent_stream_file" '^id: ' 5; then
		delivery_id="$(awk -F': ' '/^id: / {print $2; exit}' "$agent_stream_file")"
		if [ -n "$delivery_id" ]; then
			api_request POST /v1/agent/stream/ack "$agent_a_token" "$(jq -nc --arg delivery_id "$delivery_id" '{delivery_id:$delivery_id}')"
			assert_status "agent stream ack" 200 || true
		else
			fail "agent stream ack (missing delivery id)"
		fi
		check_stream "agent stream" "$agent_stream_file" 'event: (stream\.hello|room\.turn_ready|room\.state_changed)'
	else
		fail "agent stream"
		echo "stream output:"
		cat "$agent_stream_file" || true
	fi
fi

api_request GET /v1/agent/actionable-rooms "$agent_a_token"
assert_status "agent actionable rooms" 200 || true

api_request GET /v1/rooms/"$room_id"/state "$agent_a_token"
assert_status "room state" 200 || true
assert_json "room state active" '.state == "ACTIVE"' || true

api_request POST /v1/rooms/"$room_id"/access-token "$agent_a_token" "{}"
assert_status "room access token A" 201 || true
room_token_a="$(json_get "$API_BODY_FILE" '.token // empty' || true)"

api_request POST /v1/rooms/"$room_id"/access-token "$agent_b_token" "{}"
assert_status "room access token B" 201 || true
room_token_b="$(json_get "$API_BODY_FILE" '.token // empty' || true)"

api_request GET /v1/rooms/"$room_id"/context "$room_token_a"
assert_status "room context A" 200 || true
assert_json "context requires ack" '.context_ack_required == true' || true
turn_index_a="$(json_get "$API_BODY_FILE" '.turn_index' || true)"
bundle_hash_a="$(json_get "$API_BODY_FILE" '.bundle_hash' || true)"

api_request POST /v1/rooms/"$room_id"/context/ack "$room_token_a" "$(jq -nc --argjson turn_index "${turn_index_a:-0}" '{turn_index:$turn_index}')"
assert_status "context ack A" 200 || true

api_request POST /v1/rooms/"$room_id"/typing "$room_token_a" "$(jq -nc --arg state start '{state:$state,ttl_ms:30000}')"
assert_status "typing start A" 200 || true

api_request POST /v1/rooms/"$room_id"/messages "$room_token_a" "$(jq -nc --argjson expected_turn 0 --arg ciphertext 'hello from agent a' --arg bundle_hash "${bundle_hash_a:-}" '{expected_turn:$expected_turn,ciphertext:$ciphertext,bundle_hash:$bundle_hash}')"
assert_status "send message A" 201 || true

api_request POST /v1/rooms/"$room_id"/typing "$room_token_a" "$(jq -nc --arg state stop '{state:$state}')"
assert_status "typing stop A" 200 || true

api_request GET /v1/rooms/"$room_id"/state "$agent_b_token"
assert_status "room state after A" 200 || true
assert_json "turn advanced after A" '.turn_index == 1' || true

api_request GET /v1/rooms/"$room_id"/context "$room_token_b"
assert_status "room context B" 200 || true
turn_index_b="$(json_get "$API_BODY_FILE" '.turn_index' || true)"
bundle_hash_b="$(json_get "$API_BODY_FILE" '.bundle_hash' || true)"
api_request POST /v1/rooms/"$room_id"/context/ack "$room_token_b" "$(jq -nc --argjson turn_index "${turn_index_b:-0}" '{turn_index:$turn_index}')"
assert_status "context ack B" 200 || true

api_request POST /v1/rooms/"$room_id"/typing "$room_token_b" "$(jq -nc --arg state start '{state:$state,ttl_ms:30000}')"
assert_status "typing start B" 200 || true

api_request POST /v1/rooms/"$room_id"/messages "$room_token_b" "$(jq -nc --argjson expected_turn 1 --arg ciphertext 'hello from agent b' --arg bundle_hash "${bundle_hash_b:-}" '{expected_turn:$expected_turn,ciphertext:$ciphertext,bundle_hash:$bundle_hash}')"
assert_status "send message B" 201 || true

api_request POST /v1/rooms/"$room_id"/typing "$room_token_b" "$(jq -nc --arg state stop '{state:$state}')"
assert_status "typing stop B" 200 || true

api_request POST /v1/rooms/"$room_id"/viewers "" "$(jq -nc --arg human_code "$human_code" '{op:"join",human_code:$human_code}')"
assert_status "viewer join" 201 || true
viewer_token="$(json_get "$API_BODY_FILE" '.viewer_token // empty' || true)"

if [ -n "${viewer_token:-}" ]; then
	stream_info="$(start_stream_capture viewer_stream "/v1/rooms/$room_id/viewer-events" "$viewer_token")"
	viewer_stream_pid="${stream_info%%|*}"
	viewer_stream_file="${stream_info#*|}"
	if wait_for_pattern "$viewer_stream_file" 'retry: 3000|event: (typing\.start|agent\.typing)' 5; then
		check_stream "viewer events" "$viewer_stream_file" 'retry: 3000|event: (typing\.start|agent\.typing)'
	else
		fail "viewer events"
		echo "stream output:"
		cat "$viewer_stream_file" || true
	fi
	api_request POST /v1/rooms/"$room_id"/viewers "" "$(jq -nc --arg viewer_token "$viewer_token" '{op:"heartbeat",viewer_token:$viewer_token}')"
	assert_status "viewer heartbeat" 200 || true
	api_request POST /v1/rooms/"$room_id"/typing "$room_token_a" "$(jq -nc --arg state start '{state:$state,ttl_ms:30000}')"
	assert_status "typing start A (viewer)" 200 || true
	if wait_for_pattern "$viewer_stream_file" 'event: (typing\.start|agent\.typing)' 5; then
		check_stream "viewer typing event" "$viewer_stream_file" 'event: (typing\.start|agent\.typing)'
	else
		fail "viewer typing event"
		echo "stream output:"
		cat "$viewer_stream_file" || true
	fi
	api_request POST /v1/rooms/"$room_id"/typing "$room_token_a" "$(jq -nc --arg state stop '{state:$state}')"
	assert_status "typing stop A (viewer)" 200 || true
	api_request POST /v1/rooms/"$room_id"/viewers "" "$(jq -nc --arg viewer_token "$viewer_token" '{op:"leave",viewer_token:$viewer_token}')"
	assert_status "viewer leave" 200 || true
fi

stream_info="$(start_stream_capture room_events "/v1/rooms/$room_id/events" "$agent_a_token")"
room_events_pid="${stream_info%%|*}"
room_events_file="${stream_info#*|}"
if wait_for_pattern "$room_events_file" 'event: (room\.state_changed|room\.turn_ready|room\.message_created)' 5; then
	check_stream "room events stream" "$room_events_file" 'event: (room\.state_changed|room\.turn_ready|room\.message_created)'
else
	fail "room events stream"
	echo "stream output:"
	cat "$room_events_file" || true
fi

api_request GET "/v1/rooms/$room_id/events/history?since=0&limit=50" "$agent_a_token"
assert_status "room events history" 200 || true

api_request GET /v1/agent/webhooks "$agent_a_token"
assert_status "webhooks list (empty)" 200 || true

webhook_url="https://example.com/areyouai-webhook"
webhook_secret="secret-$(date +%s)"
api_request POST /v1/agent/webhooks "$agent_a_token" "$(jq -nc --arg url "$webhook_url" --arg secret "$webhook_secret" '{url:$url,secret:$secret,enabled:true}')"
assert_status "webhooks create" 201 || true
webhook_id="$(json_get "$API_BODY_FILE" '.endpoint_id // .id // empty' || true)"

api_request GET /v1/agent/webhooks "$agent_a_token"
assert_status "webhooks list (created)" 200 || true

if [ -n "${webhook_id:-}" ]; then
	api_request DELETE "/v1/agent/webhooks/$webhook_id" "$agent_a_token"
	assert_status "webhooks delete" 204 || true
else
	fail "webhooks delete (missing endpoint id)"
fi

api_request GET /v1/agent/webhooks "$agent_a_token"
assert_status "webhooks list (after delete)" 200 || true

api_request POST /v1/rooms/"$room_id"/leave "$agent_a_token" "{}"
assert_status "room leave unsupported" 501 || true

api_request POST /v1/rooms/"$room_id"/close "$agent_a_token" "{}"
assert_status "room close" 200 || true

api_request POST /v1/rooms/"$room_id"/transcript "" "$(jq -nc --arg human_code "$human_code" '{human_code:$human_code}')"
assert_status "transcript" 200 || true
assert_json "transcript contains messages" '.messages | length >= 2' || true

api_request GET /v1/capabilities
assert_status "capabilities recheck" 200 || true

echo
echo "log file: $LOG_FILE"
if [ "$FAILURES" -eq 0 ]; then
	echo "all API checks passed"
	exit 0
fi

echo "$FAILURES API checks failed"
exit 1

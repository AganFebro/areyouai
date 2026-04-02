"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const os = require("node:os");
const path = require("node:path");

const {
  BridgeDaemon,
  createLogger,
  defaultConfig,
  parseSSE
} = require("../src/bridge");

function jsonResponse(status, body) {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: new Map([["content-type", "application/json"]]),
    async text() {
      return JSON.stringify(body);
    }
  };
}

async function runTest(name, fn) {
  try {
    await fn();
    console.log(`ok - ${name}`);
  } catch (err) {
    console.error(`not ok - ${name}`);
    console.error(err && err.stack ? err.stack : err);
    process.exitCode = 1;
  }
}

async function testParseSSE() {
  async function* body() {
    yield Buffer.from("event: stream.hello\n");
    yield Buffer.from("data: {\"type\":\"stream.hello\",\"resume_status\":\"fresh\"}\n\n");
    yield Buffer.from("id: dly_1\nevent: room.turn_ready\n");
    yield Buffer.from("data: {\"delivery_id\":\"dly_1\",\"room_id\":\"room_1\"}\n\n");
  }

  const events = [];
  for await (const event of parseSSE(body())) {
    events.push(event);
  }

  assert.equal(events.length, 2);
  assert.equal(events[0].event, "stream.hello");
  assert.equal(events[0].data.resume_status, "fresh");
  assert.equal(events[1].id, "dly_1");
  assert.equal(events[1].event, "room.turn_ready");
  assert.equal(events[1].data.room_id, "room_1");
}

async function testHandleTurnReady() {
  const tmpDir = await fs.mkdtemp(path.join(os.tmpdir(), "aya-bridge-"));
  const markers = [];
  let wakePayload = null;
  const config = defaultConfig(tmpDir);
  config.aya.api_base_url = "https://api.example.test";
  config.openclaw.hook_url = "http://127.0.0.1:18789/hooks/agent";
  config.openclaw.hook_token = "oc_hook_test";
  config.openclaw.agent_id = "main";

  const fetchImpl = async (input, options = {}) => {
    const url = String(input);
    if (url === "https://api.example.test/v1/rooms/room_1/access-token" && options.method === "POST") {
      markers.push("token");
      return jsonResponse(201, {
        room_id: "room_1",
        agent_id: "agt_test",
        token: "rat_token_1",
        scope: "room:automation",
        expires_at: new Date(Date.now() + 300000).toISOString()
      });
    }
    if (url === "https://api.example.test/v1/agent/stream/ack" && options.method === "POST") {
      markers.push("ack");
      return jsonResponse(200, { status: "acked" });
    }
    if (url === "http://127.0.0.1:18789/hooks/agent" && options.method === "POST") {
      markers.push("wake");
      wakePayload = JSON.parse(String(options.body || "{}"));
      return jsonResponse(200, { ok: true });
    }
    throw new Error(`unexpected fetch ${options.method || "GET"} ${url}`);
  };

  const bridge = new BridgeDaemon({
    config,
    fetchImpl,
    logger: createLogger("error"),
    session: {
      api_key: "aya_api_test",
      session_token: "as_test",
      agent_id: "agt_test"
    },
    state: {
      last_acknowledged_delivery_id: "",
      last_connected_at: null,
      last_stream_status: "idle"
    }
  });
  await bridge.ensureLayout();

  await bridge.handleTurnReady({
    type: "room.turn_ready",
    delivery_id: "dly_1",
    room_id: "room_1",
    next_turn: 0,
    next_actor_id: "agt_test"
  });

  const tokenPath = path.join(bridge.paths.tokenDir, "room_1.json");
  const token = JSON.parse(await fs.readFile(tokenPath, "utf8"));
  assert.equal(token.token, "rat_token_1");

  const wakeFiles = await fs.readdir(bridge.paths.wakeQueueDir);
  assert.equal(wakeFiles.length, 0);
  assert.deepEqual(markers, ["token", "ack", "wake"]);
  assert.equal(wakePayload.agentId, "main");
  assert.equal(wakePayload.name, "areyouai");
  assert.ok(String(wakePayload.message || "").startsWith("[AYA_WAKE_V1]\n"));
  const contract = JSON.parse(String(wakePayload.message).split("\n").slice(1).join("\n"));
  assert.equal(contract.contract, "aya.wake.v1");
  assert.equal(contract.delivery_id, "dly_1");
  assert.equal(contract.room_id, "room_1");
  assert.equal(contract.next_turn, 0);
  assert.equal(contract.next_actor_id, "agt_test");
  assert.ok(String(contract.token_path || "").includes("/tokens/room_1.json"));

  const state = JSON.parse(await fs.readFile(bridge.paths.statePath, "utf8"));
  assert.equal(state.last_acknowledged_delivery_id, "dly_1");
}

async function testWakeQueueRetry() {
  const tmpDir = await fs.mkdtemp(path.join(os.tmpdir(), "aya-bridge-retry-"));
  let wakeAttempts = 0;
  const markers = [];
  const config = defaultConfig(tmpDir);
  config.openclaw.hook_url = "http://127.0.0.1:18789/hooks/agent";
  config.openclaw.hook_token = "oc_hook_test";

  const fetchImpl = async (input, options = {}) => {
    const url = String(input);
    if (url === "http://127.0.0.1:18789/hooks/agent" && options.method === "POST") {
      wakeAttempts += 1;
      markers.push(`wake-${wakeAttempts}`);
      if (wakeAttempts === 1) {
        return jsonResponse(500, { error: "temporary" });
      }
      return jsonResponse(200, { ok: true });
    }
    throw new Error(`unexpected fetch ${options.method || "GET"} ${url}`);
  };

  const bridge = new BridgeDaemon({
    config,
    fetchImpl,
    logger: createLogger("error"),
    session: {},
    state: {
      last_acknowledged_delivery_id: "dly_existing",
      last_connected_at: null,
      last_stream_status: "idle"
    }
  });
  await bridge.ensureLayout();
  await bridge.writeRoomToken("room_retry", {
    room_id: "room_retry",
    agent_id: "agt_test",
    token: "rat_retry",
    expires_at: new Date(Date.now() + 300000).toISOString(),
    scope: "room:automation"
  });
  await bridge.enqueueWakeJob({
    delivery_id: "dly_retry",
    type: "room.turn_ready",
    room_id: "room_retry",
    received_at: new Date().toISOString()
  });

  await bridge.drainWakeQueue();
  let wakeFiles = await fs.readdir(bridge.paths.wakeQueueDir);
  assert.equal(wakeFiles.length, 1);

  await bridge.drainWakeQueue();
  wakeFiles = await fs.readdir(bridge.paths.wakeQueueDir);
  assert.equal(wakeFiles.length, 0);
  assert.deepEqual(markers, ["wake-1", "wake-2"]);
}

async function main() {
  const mode = String(process.argv[2] || "all").trim();
  if (mode === "parse" || mode === "all") {
    await runTest("parseSSE yields event payloads", testParseSSE);
  }
  if (mode === "turn" || mode === "all") {
    await runTest("handleTurnReady writes token, acks, then wakes", testHandleTurnReady);
  }
  if (mode === "retry" || mode === "all") {
    await runTest("wake queue retries pending jobs", testWakeQueueRetry);
  }
  if (process.exitCode) {
    process.exit(process.exitCode);
  }
}

main().catch((err) => {
  console.error(err && err.stack ? err.stack : err);
  process.exit(1);
});

"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const os = require("node:os");
const path = require("node:path");
const { setTimeout: sleep } = require("node:timers/promises");

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
  assert.deepEqual(markers, ["token", "ack", "wake", "ack"]);
  assert.equal(wakePayload.agentId, "main");
  assert.equal(wakePayload.name, "areyouai");
  assert.ok(String(wakePayload.message || "").startsWith("[AYA_WAKE_V1]\n"));
  const contract = JSON.parse(String(wakePayload.message).split("\n").slice(1).join("\n"));
  assert.equal(contract.contract, "aya.wake.v1");
  assert.equal(contract.delivery_id, "dly_1");
  assert.equal(contract.room_id, "room_1");
  assert.equal(contract.next_turn, 0);
  assert.equal(contract.next_actor_id, "agt_test");
  const contractTokenPath = String(contract.token_path || "");
  assert.equal(path.basename(contractTokenPath), "room_1.json");
  assert.equal(path.basename(path.dirname(contractTokenPath)), "tokens");

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

async function testWakeRefreshesNearExpiryRoomToken() {
  const tmpDir = await fs.mkdtemp(path.join(os.tmpdir(), "aya-bridge-refresh-"));
  const markers = [];
  let wakePayload = null;
  const config = defaultConfig(tmpDir);
  config.aya.api_base_url = "https://api.example.test";
  config.openclaw.hook_url = "http://127.0.0.1:18789/hooks/agent";
  config.openclaw.hook_token = "oc_hook_test";
  config.aya.token_refresh_threshold_seconds = 60;

  const expiringSoon = new Date(Date.now() + 15_000).toISOString();
  const refreshedExpiry = new Date(Date.now() + 300_000).toISOString();

  const fetchImpl = async (input, options = {}) => {
    const url = String(input);
    if (url === "https://api.example.test/v1/rooms/room_refresh/access-token" && options.method === "POST") {
      markers.push("refresh");
      return jsonResponse(201, {
        room_id: "room_refresh",
        agent_id: "agt_test",
        token: "rat_refreshed",
        scope: "room:automation",
        expires_at: refreshedExpiry
      });
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
  await bridge.writeRoomToken("room_refresh", {
    room_id: "room_refresh",
    agent_id: "agt_test",
    token: "rat_old",
    expires_at: expiringSoon,
    scope: "room:automation"
  });

  await bridge.wakeOpenClaw({
    delivery_id: "dly_refresh",
    type: "room.turn_ready",
    room_id: "room_refresh",
    next_turn: 2,
    next_actor_id: "agt_test",
    token_expires_at: expiringSoon
  });

  assert.deepEqual(markers, ["refresh", "wake"]);
  assert.ok(wakePayload, "wake payload should be present");
  const contract = JSON.parse(String(wakePayload.message).split("\n").slice(1).join("\n"));
  assert.equal(contract.token_expires_at, refreshedExpiry);

  const tokenPath = path.join(bridge.paths.tokenDir, "room_refresh.json");
  const stored = JSON.parse(await fs.readFile(tokenPath, "utf8"));
  assert.equal(stored.token, "rat_refreshed");
}

async function testServeDowngradesExpectedDisconnectLogs() {
  const tmpDir = await fs.mkdtemp(path.join(os.tmpdir(), "aya-bridge-disconnect-"));
  const logs = { info: [], warn: [] };
  const config = defaultConfig(tmpDir);
  config.aya.api_base_url = "https://api.example.test";
  config.aya.reconnect.base_delay_ms = 1;
  config.aya.reconnect.max_delay_ms = 1;
  config.aya.reconnect.jitter_ms = 0;

  const fetchImpl = async (input, options = {}) => {
    const url = String(input);
    if (url === "https://api.example.test/v1/agent/stream" && options.method === "GET") {
      async function* body() {
        yield Buffer.from("event: stream.hello\n");
        yield Buffer.from("data: {\"type\":\"stream.hello\",\"resume_status\":\"fresh\"}\n\n");
        throw new Error("terminated");
      }
      return {
        ok: true,
        status: 200,
        headers: new Map([["content-type", "text/event-stream"]]),
        body: body(),
        async text() {
          return "";
        }
      };
    }
    throw new Error(`unexpected fetch ${options.method || "GET"} ${url}`);
  };

  const bridge = new BridgeDaemon({
    config,
    fetchImpl,
    logger: {
      debug: (...args) => logs.info.push(args.join(" ")),
      info: (...args) => logs.info.push(args.join(" ")),
      warn: (...args) => logs.warn.push(args.join(" ")),
      error: (...args) => logs.warn.push(args.join(" "))
    },
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

  const controller = new AbortController();
  const servePromise = bridge.serve(controller.signal);
  await sleep(20);
  controller.abort();
  await servePromise;

  assert.ok(logs.info.some((line) => line.includes("stream disconnected error=terminated")));
  assert.ok(!logs.warn.some((line) => line.includes("stream disconnected error=terminated")));
}

async function testWakeEmitsTypingPulse() {
  const tmpDir = await fs.mkdtemp(path.join(os.tmpdir(), "aya-bridge-typing-"));
  const markers = [];
  let hookPayload = null;
  const config = defaultConfig(tmpDir);
  config.aya.api_base_url = "https://api.example.test";
  config.openclaw.hook_url = "http://127.0.0.1:18789/hooks/agent";
  config.openclaw.hook_token = "oc_hook_test";

  const fetchImpl = async (input, options = {}) => {
    const url = String(input);
    if (url === "https://api.example.test/v1/rooms/room_typing/access-token" && options.method === "POST") {
      markers.push("token");
      return jsonResponse(201, {
        room_id: "room_typing",
        agent_id: "agt_test",
        token: "rat_typing",
        scope: "room:automation",
        expires_at: new Date(Date.now() + 300000).toISOString()
      });
    }
    if (url === "https://api.example.test/v1/rooms/room_typing/typing" && options.method === "POST") {
      const payload = JSON.parse(String(options.body || "{}"));
      markers.push(payload.state === "start" ? "typing-start" : "typing-stop");
      assert.equal(String(options.headers?.Authorization || ""), "Bearer rat_typing");
      if (payload.state === "start") {
        assert.equal(payload.ttl_ms, 30000);
      }
      return jsonResponse(200, { ok: true });
    }
    if (url === "http://127.0.0.1:18789/hooks/agent" && options.method === "POST") {
      markers.push("wake");
      hookPayload = JSON.parse(String(options.body || "{}"));
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

  await bridge.wakeOpenClaw({
    delivery_id: "dly_typing",
    type: "room.turn_ready",
    room_id: "room_typing",
    next_turn: 4,
    next_actor_id: "agt_test"
  });

  assert.deepEqual(markers, ["token", "typing-start", "wake", "typing-stop"]);
  assert.ok(hookPayload, "wake payload should be present");
  const contract = JSON.parse(String(hookPayload.message).split("\n").slice(1).join("\n"));
  assert.equal(contract.room_id, "room_typing");
}

async function testEnqueueWakeJobDedupesEquivalentTurnReady() {
  const tmpDir = await fs.mkdtemp(path.join(os.tmpdir(), "aya-bridge-dedupe-"));
  const bridge = new BridgeDaemon({
    config: defaultConfig(tmpDir),
    logger: createLogger("error"),
    session: {},
    state: {}
  });
  await bridge.ensureLayout();

  await bridge.enqueueWakeJob({
    delivery_id: "dly_one",
    type: "room.turn_ready",
    room_id: "room_dedupe",
    next_turn: 5,
    next_actor_id: "agt_x",
    received_at: new Date().toISOString()
  });
  await bridge.enqueueWakeJob({
    delivery_id: "dly_two",
    type: "room.turn_ready",
    room_id: "room_dedupe",
    next_turn: 5,
    next_actor_id: "agt_x",
    received_at: new Date().toISOString()
  });

  const files = await fs.readdir(bridge.paths.wakeQueueDir);
  assert.equal(files.length, 1);
  assert.ok(files[0].includes("dly_one"), `expected first delivery file, got ${files[0]}`);

  bridge.rememberCompletedWakeKey("room_dedupe|5|agt_x");
  await bridge.saveState();
  const skipped = await bridge.enqueueWakeJob({
    delivery_id: "dly_three",
    type: "room.turn_ready",
    room_id: "room_dedupe",
    next_turn: 5,
    next_actor_id: "agt_x",
    received_at: new Date().toISOString()
  });
  assert.equal(skipped, "");
  const filesAfter = await fs.readdir(bridge.paths.wakeQueueDir);
  assert.equal(filesAfter.length, 1);
}

async function testProcessRecoveryClearsTerminalWakeJobs() {
  const tmpDir = await fs.mkdtemp(path.join(os.tmpdir(), "aya-bridge-recovery-"));
  const markers = [];
  const config = defaultConfig(tmpDir);
  config.aya.api_base_url = "https://api.example.test";

  const fetchImpl = async (input, options = {}) => {
    const url = String(input);
    if (url === "https://api.example.test/v1/agent/actionable-rooms") {
      markers.push("recovery");
      return jsonResponse(200, {
        actionable: [],
        terminal: [
          {
            room_id: "room_terminal",
            room_state: "CLOSED"
          }
        ]
      });
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
      last_acknowledged_delivery_id: "dly_old",
      last_connected_at: null,
      last_stream_status: "connected"
    }
  });
  await bridge.ensureLayout();
  await bridge.writeRoomToken("room_terminal", {
    room_id: "room_terminal",
    agent_id: "agt_test",
    token: "rat_old",
    expires_at: new Date(Date.now() + 300000).toISOString(),
    scope: "room:automation"
  });
  bridge.rememberCompletedWakeKey("room_terminal|9|agt_test");
  await bridge.saveState();
  await bridge.enqueueWakeJob({
    delivery_id: "dly_terminal",
    type: "room.turn_ready",
    room_id: "room_terminal",
    next_turn: 9,
    next_actor_id: "agt_test",
    received_at: new Date().toISOString()
  });

  await bridge.processRecovery();

  assert.deepEqual(markers, ["recovery"]);
  assert.equal(bridge.state.last_acknowledged_delivery_id, "");
  const tokenPath = path.join(bridge.paths.tokenDir, "room_terminal.json");
  const tokenExists = await fs.access(tokenPath).then(() => true).catch(() => false);
  assert.equal(tokenExists, false);
  const wakeFiles = await fs.readdir(bridge.paths.wakeQueueDir);
  assert.equal(wakeFiles.length, 0);
  const state = JSON.parse(await fs.readFile(bridge.paths.statePath, "utf8"));
  assert.ok(!state.completed_wake_keys.includes("room_terminal|9|agt_test"));
}

async function testProcessRecoveryRetainsCursorOnFailure() {
  const tmpDir = await fs.mkdtemp(path.join(os.tmpdir(), "aya-bridge-recovery-fail-"));
  const markers = [];
  let streamURL = null;
  const config = defaultConfig(tmpDir);
  config.aya.api_base_url = "https://api.example.test";
  const fetchImpl = async (input, options = {}) => {
    const url = String(input);
    if (url === "https://api.example.test/v1/agent/actionable-rooms") {
      markers.push("recovery-fail");
      return jsonResponse(500, { error: "temporary" });
    }
    if (url.startsWith("https://api.example.test/v1/agent/stream") && options.method === "GET") {
      streamURL = url;
      return {
        ok: true,
        status: 200,
        headers: new Map([[
          "content-type",
          "text/event-stream"
        ]]),
        body: {
          async getReader() {
            return {
              async read() {
                return { done: true, value: undefined };
              },
              releaseLock() {}
            };
          }
        },
        async text() {
          return "";
        }
      };
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
      last_acknowledged_delivery_id: "dly_old",
      last_connected_at: null,
      last_stream_status: "connected"
    }
  });
  await bridge.ensureLayout();

  await assert.rejects(() => bridge.processRecovery(), /500/);
  assert.deepEqual(markers, ["recovery-fail"]);
  assert.equal(bridge.state.last_acknowledged_delivery_id, "dly_old");

  await bridge.openStream(new AbortController().signal);
  assert.ok(streamURL, "stream URL should be captured");
  assert.ok(streamURL.includes("last_delivery_id=dly_old"), streamURL);
}

async function testHandleTerminalClearsPendingWakeJobs() {
  const tmpDir = await fs.mkdtemp(path.join(os.tmpdir(), "aya-bridge-terminal-"));
  const markers = [];
  const fetchImpl = async (input, options = {}) => {
    const url = String(input);
    if (url === "https://api.example.test/v1/agent/stream/ack" && options.method === "POST") {
      markers.push("ack");
      return jsonResponse(200, { status: "acked" });
    }
    throw new Error(`unexpected fetch ${options.method || "GET"} ${url}`);
  };
  const config = defaultConfig(tmpDir);
  config.aya.api_base_url = "https://api.example.test";
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
      last_stream_status: "idle",
      completed_wake_keys: []
    }
  });
  await bridge.ensureLayout();
  await bridge.writeRoomToken("room_terminal", {
    room_id: "room_terminal",
    agent_id: "agt_test",
    token: "rat_old",
    expires_at: new Date(Date.now() + 300000).toISOString(),
    scope: "room:automation"
  });
  await bridge.enqueueWakeJob({
    delivery_id: "dly_pending",
    type: "room.turn_ready",
    room_id: "room_terminal",
    next_turn: 9,
    next_actor_id: "agt_test",
    received_at: new Date().toISOString()
  });

  await bridge.handleTerminal({
    type: "room.closed",
    delivery_id: "dly_closed",
    room_id: "room_terminal"
  });

  const tokenExists = await fs.access(path.join(bridge.paths.tokenDir, "room_terminal.json")).then(() => true).catch(() => false);
  assert.equal(tokenExists, false);
  const wakeFiles = await fs.readdir(bridge.paths.wakeQueueDir);
  assert.equal(wakeFiles.length, 0);
  assert.deepEqual(markers, ["ack"]);
}

async function main() {
  const mode = String(process.argv[2] || "all").trim();
  if (mode === "parse" || mode === "all") {
    await runTest("parseSSE yields event payloads", testParseSSE);
  }
  if (mode === "turn" || mode === "all") {
    await runTest("handleTurnReady dedupes duplicate turn_ready deliveries", testHandleTurnReady);
  }
  if (mode === "retry" || mode === "all") {
    await runTest("wake queue retries pending jobs", testWakeQueueRetry);
  }
  if (mode === "refresh" || mode === "all") {
    await runTest("wake refreshes near-expiry room token", testWakeRefreshesNearExpiryRoomToken);
  }
  if (mode === "disconnect" || mode === "all") {
    await runTest("serve downgrades expected disconnect logs", testServeDowngradesExpectedDisconnectLogs);
  }
  if (mode === "typing" || mode === "all") {
    await runTest("wake emits typing pulse around hook execution", testWakeEmitsTypingPulse);
  }
  if (mode === "dedupe" || mode === "all") {
    await runTest("enqueueWakeJob dedupes equivalent room.turn_ready jobs", testEnqueueWakeJobDedupesEquivalentTurnReady);
  }
  if (mode === "recovery" || mode === "all") {
    await runTest("processRecovery clears terminal room wake jobs", testProcessRecoveryClearsTerminalWakeJobs);
  }
  if (mode === "recovery-failure" || mode === "all") {
    await runTest("processRecovery retains cursor on failure", testProcessRecoveryRetainsCursorOnFailure);
  }
  if (mode === "terminal" || mode === "all") {
    await runTest("handleTerminal clears pending wake jobs", testHandleTerminalClearsPendingWakeJobs);
  }
  if (process.exitCode) {
    process.exit(process.exitCode);
  }
}

main().catch((err) => {
  console.error(err && err.stack ? err.stack : err);
  process.exit(1);
});

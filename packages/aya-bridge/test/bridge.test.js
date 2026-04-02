"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const http = require("node:http");
const os = require("node:os");
const path = require("node:path");

const {
  BridgeDaemon,
  createLogger,
  defaultConfig,
  parseSSE
} = require("../src/bridge");

async function withServer(handler, fn) {
  const server = http.createServer(handler);
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  try {
    const address = server.address();
    return await fn(`http://${address.address}:${address.port}`);
  } finally {
    await new Promise((resolve, reject) => server.close((err) => (err ? reject(err) : resolve())));
  }
}

test("parseSSE yields event payloads", async () => {
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
});

test("handleTurnReady writes token, acks, then wakes", async () => {
  const tmpDir = await fs.mkdtemp(path.join(os.tmpdir(), "aya-bridge-"));
  const markers = [];

  await withServer(async (req, res) => {
    if (req.method === "POST" && req.url === "/v1/rooms/room_1/access-token") {
      markers.push("token");
      res.writeHead(201, { "Content-Type": "application/json" });
      res.end(
        JSON.stringify({
          room_id: "room_1",
          agent_id: "agt_test",
          token: "rat_token_1",
          scope: "room:automation",
          expires_at: new Date(Date.now() + 300000).toISOString()
        })
      );
      return;
    }
    if (req.method === "POST" && req.url === "/v1/agent/stream/ack") {
      markers.push("ack");
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ status: "acked" }));
      return;
    }
    res.writeHead(404).end();
  }, async (ayaBaseURL) => {
    await withServer(async (req, res) => {
      if (req.method === "POST" && req.url === "/hooks/agent") {
        markers.push("wake");
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ ok: true }));
        return;
      }
      res.writeHead(404).end();
    }, async (hookURL) => {
      const config = defaultConfig(tmpDir);
      config.aya.api_base_url = ayaBaseURL;
      config.openclaw.hook_url = `${hookURL}/hooks/agent`;
      config.openclaw.hook_token = "oc_hook_test";
      config.openclaw.agent_id = "main";
      const bridge = new BridgeDaemon({
        config,
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

      const state = JSON.parse(await fs.readFile(bridge.paths.statePath, "utf8"));
      assert.equal(state.last_acknowledged_delivery_id, "dly_1");
    });
  });
});

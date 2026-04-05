"use strict";

const crypto = require("node:crypto");
const fs = require("node:fs/promises");
const path = require("node:path");
const os = require("node:os");
const { setTimeout: sleep } = require("node:timers/promises");

const DEFAULT_BASE_DIR = path.join(os.homedir(), ".areyouai");
const DEFAULT_TYPING_TTL_MS = 30_000;

class HTTPError extends Error {
  constructor(status, body, message) {
    super(message || `HTTP ${status}`);
    this.name = "HTTPError";
    this.status = status;
    this.body = body;
  }
}

function defaultConfig(baseDir = DEFAULT_BASE_DIR) {
  const dir = expandHome(baseDir);
  return {
    aya: {
      api_base_url: "https://api.areyouai.fun",
      token_refresh_threshold_seconds: 60,
      reconnect: {
        base_delay_ms: 1000,
        max_delay_ms: 10000,
        jitter_ms: 250
      },
      wake_retry_interval_ms: 5000
    },
    openclaw: {
      hook_url: "http://127.0.0.1:18789/hooks/agent",
      hook_token: "",
      agent_id: "main"
    },
    storage: {
      base_dir: dir,
      token_dir: path.join(dir, "tokens"),
      wake_queue_dir: path.join(dir, "wake-queue"),
      log_dir: path.join(dir, "logs")
    }
  };
}

function expandHome(inputPath) {
  if (!inputPath) {
    return inputPath;
  }
  if (inputPath === "~") {
    return os.homedir();
  }
  if (inputPath.startsWith("~/")) {
    return path.join(os.homedir(), inputPath.slice(2));
  }
  return inputPath;
}

function resolvePaths(config) {
  const baseDir = expandHome(config.storage?.base_dir || DEFAULT_BASE_DIR);
  return {
    baseDir,
    configPath: path.join(baseDir, "config.json"),
    sessionPath: path.join(baseDir, "session.json"),
    statePath: path.join(baseDir, "state.json"),
    tokenDir: expandHome(config.storage?.token_dir || path.join(baseDir, "tokens")),
    wakeQueueDir: expandHome(config.storage?.wake_queue_dir || path.join(baseDir, "wake-queue")),
    logDir: expandHome(config.storage?.log_dir || path.join(baseDir, "logs"))
  };
}

function createLogger(level = "info") {
  const weights = { debug: 10, info: 20, warn: 30, error: 40 };
  const threshold = weights[level] || weights.info;
  const emit = (name, args) => {
    if ((weights[name] || weights.info) < threshold) {
      return;
    }
    const line = [`[${new Date().toISOString()}]`, name.toUpperCase(), ...args].join(" ");
    if (name === "error") {
      console.error(line);
      return;
    }
    console.log(line);
  };
  return {
    debug: (...args) => emit("debug", args),
    info: (...args) => emit("info", args),
    warn: (...args) => emit("warn", args),
    error: (...args) => emit("error", args)
  };
}

async function pathExists(target) {
  try {
    await fs.access(target);
    return true;
  } catch {
    return false;
  }
}

async function ensureDir(target) {
  await fs.mkdir(target, { recursive: true, mode: 0o700 });
}

async function atomicWriteJSON(target, value) {
  const dir = path.dirname(target);
  await ensureDir(dir);
  const tmp = path.join(
    dir,
    `.${path.basename(target)}.${process.pid}.${Date.now()}.${crypto.randomUUID()}.tmp`
  );
  const body = `${JSON.stringify(value, null, 2)}\n`;
  const handle = await fs.open(tmp, "w", 0o600);
  try {
    await handle.writeFile(body);
    await handle.sync();
  } finally {
    await handle.close().catch(() => {});
  }
  await fs.rename(tmp, target);
  try {
    const dirHandle = await fs.open(dir, "r");
    try {
      await dirHandle.sync();
    } finally {
      await dirHandle.close().catch(() => {});
    }
  } catch {
    // Best-effort directory sync.
  }
}

async function readJSON(target, fallback = null) {
  try {
    const raw = await fs.readFile(target, "utf8");
    return JSON.parse(raw);
  } catch (err) {
    if (err && err.code === "ENOENT") {
      return fallback;
    }
    throw err;
  }
}

async function removeIfExists(target) {
  await fs.rm(target, { force: true });
}

async function listJSONFiles(dir) {
  if (!(await pathExists(dir))) {
    return [];
  }
  const entries = await fs.readdir(dir, { withFileTypes: true });
  return entries
    .filter((entry) => entry.isFile() && entry.name.endsWith(".json"))
    .map((entry) => path.join(dir, entry.name))
    .sort();
}

function normalizeURL(raw) {
  return String(raw || "").trim().replace(/\/+$/, "");
}

function parseJSONText(text) {
  if (!text) {
    return {};
  }
  try {
    return JSON.parse(text);
  } catch {
    return { raw: text };
  }
}

function parseISOTime(raw) {
  const text = String(raw || "").trim();
  if (!text) {
    return null;
  }
  const at = new Date(text);
  if (Number.isNaN(at.getTime())) {
    return null;
  }
  return at;
}

function computeBackoffMs(reconnect, attempt) {
  const base = Number(reconnect?.base_delay_ms || 1000);
  const max = Number(reconnect?.max_delay_ms || 10000);
  const jitter = Number(reconnect?.jitter_ms || 0);
  const growth = Math.min(max, base * Math.pow(2, Math.max(0, attempt)));
  const jitterAdd = jitter > 0 ? Math.floor(Math.random() * (jitter + 1)) : 0;
  return growth + jitterAdd;
}

function isExpectedStreamDisconnectError(err) {
  if (!err) {
    return false;
  }
  if (err.name === "AbortError") {
    return true;
  }
  const message = String(err.message || "").trim().toLowerCase();
  if (!message) {
    return false;
  }
  return ["terminated", "eof", "end of file", "premature close", "socket closed", "connection closed"].some((needle) => message.includes(needle));
}

async function* parseSSE(body) {
  const decoder = new TextDecoder();
  let buffer = "";
  const reader = typeof body?.getReader === "function" ? body.getReader() : null;
  if (reader) {
    while (true) {
      const { done, value } = await reader.read();
      if (done) {
        break;
      }
      buffer += decoder.decode(value, { stream: true });
      yield* drainSSEBuffer(() => buffer, (next) => {
        buffer = next;
      });
    }
    buffer += decoder.decode();
    yield* drainSSEBuffer(() => buffer, (next) => {
      buffer = next;
    });
    return;
  }

  for await (const chunk of body) {
    buffer += typeof chunk === "string" ? chunk : decoder.decode(chunk, { stream: true });
    yield* drainSSEBuffer(() => buffer, (next) => {
      buffer = next;
    });
  }
  yield* drainSSEBuffer(() => buffer, (next) => {
    buffer = next;
  });
}

function* drainSSEBuffer(getBuffer, setBuffer) {
  let buffer = getBuffer();
  while (true) {
    const idx = buffer.indexOf("\n\n");
    if (idx === -1) {
      break;
    }
    const frame = buffer.slice(0, idx);
    buffer = buffer.slice(idx + 2);
    const parsed = parseSSEFrame(frame);
    if (parsed) {
      yield parsed;
    }
  }
  setBuffer(buffer);
}

function parseSSEFrame(frame) {
  const lines = frame.split(/\r?\n/);
  let id = "";
  let event = "";
  const data = [];
  for (const rawLine of lines) {
    if (!rawLine || rawLine.startsWith(":") || rawLine.startsWith("retry:")) {
      continue;
    }
    if (rawLine.startsWith("id:")) {
      id = rawLine.slice(3).trim();
      continue;
    }
    if (rawLine.startsWith("event:")) {
      event = rawLine.slice(6).trim();
      continue;
    }
    if (rawLine.startsWith("data:")) {
      data.push(rawLine.slice(5).trim());
    }
  }
  if (!event && data.length === 0) {
    return null;
  }
  return {
    id,
    event,
    data: parseJSONText(data.join("\n"))
  };
}

class BridgeDaemon {
  constructor(options) {
    this.fetch = options.fetchImpl || globalThis.fetch;
    this.logger = options.logger || createLogger(options.logLevel || "info");
    this.config = JSON.parse(JSON.stringify(options.config || defaultConfig(options.baseDir)));
    this.paths = resolvePaths(this.config);
    this.session = options.session || {};
    this.state = options.state || {
      last_acknowledged_delivery_id: "",
      last_connected_at: null,
      last_stream_status: "idle",
      completed_wake_keys: []
    };
    if (!Array.isArray(this.state.completed_wake_keys)) {
      this.state.completed_wake_keys = [];
    }
  }

  static async fromDisk(options = {}) {
    const config = await readJSON(options.configPath || path.join(expandHome(options.baseDir || DEFAULT_BASE_DIR), "config.json"));
    if (!config) {
      throw Object.assign(new Error("bridge config not found; run init first"), { exitCode: 2 });
    }
    const daemon = new BridgeDaemon({
      ...options,
      config,
      session: await readJSON(path.join(resolvePaths(config).baseDir, "session.json"), {}),
      state: await readJSON(path.join(resolvePaths(config).baseDir, "state.json"), {
        last_acknowledged_delivery_id: "",
        last_connected_at: null,
        last_stream_status: "idle",
        completed_wake_keys: []
      })
    });
    await daemon.ensureLayout();
    return daemon;
  }

  async ensureLayout() {
    await ensureDir(this.paths.baseDir);
    await ensureDir(this.paths.tokenDir);
    await ensureDir(this.paths.wakeQueueDir);
    await ensureDir(this.paths.logDir);
  }

  async saveConfig() {
    await atomicWriteJSON(this.paths.configPath, this.config);
  }

  async saveSession() {
    await atomicWriteJSON(this.paths.sessionPath, this.session);
  }

  async saveState() {
    await atomicWriteJSON(this.paths.statePath, this.state);
  }

  normalizeTurnReadyWakeKey(job) {
    if (String(job?.type || "").trim() !== "room.turn_ready") {
      return "";
    }
    const roomId = String(job?.room_id || "").trim();
    const nextActorID = String(job?.next_actor_id || "").trim();
    if (!roomId || !nextActorID || typeof job?.next_turn !== "number") {
      return "";
    }
    return `${roomId}|${job.next_turn}|${nextActorID}`;
  }

  hasCompletedWakeKey(key) {
    const normalized = String(key || "").trim();
    if (!normalized) {
      return false;
    }
    return (this.state.completed_wake_keys || []).includes(normalized);
  }

  rememberCompletedWakeKey(key) {
    const normalized = String(key || "").trim();
    if (!normalized) {
      return;
    }
    const current = Array.isArray(this.state.completed_wake_keys) ? this.state.completed_wake_keys : [];
    this.state.completed_wake_keys = [normalized, ...current.filter((item) => item !== normalized)].slice(0, 64);
  }

  forgetCompletedWakeKeysForRoom(roomId) {
    const targetRoomID = String(roomId || "").trim();
    if (!targetRoomID) {
      return;
    }
    const prefix = `${targetRoomID}|`;
    this.state.completed_wake_keys = (Array.isArray(this.state.completed_wake_keys) ? this.state.completed_wake_keys : [])
      .filter((key) => !String(key || "").startsWith(prefix));
  }

  async relogin() {
    const apiKey = String(this.session.api_key || "").trim();
    if (!apiKey) {
      throw Object.assign(new Error("AYA API key missing; run login first"), { exitCode: 3 });
    }
    const payload = await this.requestJSON("/v1/agent/login", {
      method: "POST",
      auth: "none",
      body: { api_key: apiKey }
    });
    const sessionToken = String(payload.session_token || "").trim();
    if (!sessionToken) {
      throw Object.assign(new Error("AYA login did not return session_token"), { exitCode: 3 });
    }
    this.session = {
      ...this.session,
      session_token: sessionToken,
      updated_at: new Date().toISOString()
    };
    await this.saveSession();
    return this.session;
  }

  async ensureSession() {
    if (String(this.session.session_token || "").trim()) {
      return this.session;
    }
    return this.relogin();
  }

  apiURL(pathname) {
    return new URL(pathname, `${normalizeURL(this.config.aya.api_base_url)}/`).toString();
  }

  async apiFetch(pathname, options = {}) {
    const {
      method = "GET",
      body,
      headers = {},
      auth = "session",
      retryOnUnauthorized = true,
      signal
    } = options;

    if (auth === "session") {
      await this.ensureSession();
    }
    const finalHeaders = { ...headers };
    if (auth === "session") {
      finalHeaders.Authorization = `Bearer ${this.session.session_token}`;
    }
    let payloadBody;
    if (body !== undefined) {
      finalHeaders["Content-Type"] = "application/json";
      payloadBody = JSON.stringify(body);
    }
    const response = await this.fetch(this.apiURL(pathname), {
      method,
      headers: finalHeaders,
      body: payloadBody,
      signal
    });
    if (response.status === 401 && auth === "session" && retryOnUnauthorized) {
      await this.relogin();
      return this.apiFetch(pathname, { ...options, retryOnUnauthorized: false });
    }
    return response;
  }

  async requestJSON(pathname, options = {}) {
    const response = await this.apiFetch(pathname, options);
    const text = await response.text();
    const body = parseJSONText(text);
    if (!response.ok) {
      throw new HTTPError(response.status, body, `${options.method || "GET"} ${pathname} -> ${response.status}`);
    }
    return body;
  }

  async writeRoomToken(roomId, tokenPayload) {
    const file = path.join(this.paths.tokenDir, `${roomId}.json`);
    await atomicWriteJSON(file, {
      room_id: roomId,
      agent_id: tokenPayload.agent_id || this.session.agent_id || "",
      token: tokenPayload.token,
      expires_at: tokenPayload.expires_at,
      scope: tokenPayload.scope || "room:automation",
      updated_at: new Date().toISOString()
    });
  }

  async deleteRoomToken(roomId) {
    await removeIfExists(path.join(this.paths.tokenDir, `${roomId}.json`));
  }

  async refreshRoomToken(roomId) {
    const payload = await this.requestJSON(`/v1/rooms/${encodeURIComponent(roomId)}/access-token`, {
      method: "POST"
    });
    const token = String(payload.token || "").trim();
    if (!token) {
      throw new Error(`room access token response missing token for ${roomId}`);
    }
    await this.writeRoomToken(roomId, payload);
    return payload;
  }

  async sendRoomTypingSignal(roomId, token, state, ttlMs) {
    const cleanRoomId = String(roomId || "").trim();
    const cleanToken = String(token || "").trim();
    const cleanState = String(state || "").trim();
    if (!cleanRoomId || !cleanToken || (cleanState !== "start" && cleanState !== "stop")) {
      return false;
    }
    try {
      await this.requestJSON(`/v1/rooms/${encodeURIComponent(cleanRoomId)}/typing`, {
        method: "POST",
        auth: "none",
        headers: {
          Authorization: `Bearer ${cleanToken}`
        },
        body: cleanState === "start" ? { state: cleanState, ttl_ms: ttlMs } : { state: cleanState }
      });
      return true;
    } catch (err) {
      this.logger.warn(`typing signal failed room_id=${cleanRoomId} state=${cleanState} error=${err.message}`);
      return false;
    }
  }

  async readRoomToken(roomId) {
    return readJSON(path.join(this.paths.tokenDir, `${roomId}.json`), null);
  }

  shouldRefreshToken(expiresAt) {
    const expiry = parseISOTime(expiresAt);
    if (!expiry) {
      return true;
    }
    const thresholdSeconds = Number(this.config.aya?.token_refresh_threshold_seconds || 60);
    const thresholdMs = Math.max(0, thresholdSeconds) * 1000;
    return (expiry.getTime() - Date.now()) <= thresholdMs;
  }

  async ensureFreshRoomTokenForJob(job) {
    if (String(job.type || "").trim() !== "room.turn_ready") {
      return null;
    }
    const roomId = String(job.room_id || "").trim();
    if (!roomId) {
      return null;
    }

    const current = await this.readRoomToken(roomId);
    const hasToken = String(current?.token || "").trim() !== "";
    const expiresAt = current?.expires_at || job.token_expires_at;
    const needsRefresh = !hasToken || this.shouldRefreshToken(expiresAt);
    if (!needsRefresh) {
      return current;
    }

    this.logger.info(`refreshing room token room_id=${roomId} reason=pre_wake_or_missing`);
    const refreshed = await this.refreshRoomToken(roomId);
    return this.readRoomToken(roomId).catch(() => ({
      room_id: roomId,
      token: refreshed.token,
      expires_at: refreshed.expires_at
    }));
  }

  normalizeTurnReadyWakeKey(job) {
    if (String(job.type || "").trim() !== "room.turn_ready") {
      return "";
    }
    const roomId = String(job.room_id || "").trim();
    const nextActorID = String(job.next_actor_id || "").trim();
    if (!roomId || !nextActorID || typeof job.next_turn !== "number") {
      return "";
    }
    return `${roomId}|${job.next_turn}|${nextActorID}`;
  }

  async findEquivalentPendingWakeJob(job) {
    const key = this.normalizeTurnReadyWakeKey(job);
    if (!key) {
      return "";
    }
    const files = await listJSONFiles(this.paths.wakeQueueDir);
    for (const file of files) {
      const existing = await readJSON(file, null);
      if (!existing) {
        continue;
      }
      if (String(existing.status || "pending") === "done") {
        continue;
      }
      if (this.normalizeTurnReadyWakeKey(existing) === key) {
        return file;
      }
    }
    return "";
  }

  async enqueueWakeJob(job) {
    const completedKey = this.normalizeTurnReadyWakeKey(job);
    if (completedKey && this.hasCompletedWakeKey(completedKey)) {
      return "";
    }
    const equivalent = await this.findEquivalentPendingWakeJob(job);
    if (equivalent) {
      return equivalent;
    }
    const file = path.join(this.paths.wakeQueueDir, `${job.delivery_id}.json`);
    const exists = await pathExists(file);
    if (exists) {
      return file;
    }
    await atomicWriteJSON(file, {
      delivery_id: job.delivery_id,
      type: job.type,
      room_id: job.room_id,
      event_id: job.event_id || null,
      reason: job.reason || null,
      room_state: job.room_state || null,
      next_turn: typeof job.next_turn === "number" ? job.next_turn : null,
      next_actor_id: job.next_actor_id || null,
      occurred_at: job.occurred_at || null,
      token_expires_at: job.token_expires_at || null,
      received_at: job.received_at || new Date().toISOString(),
      status: "pending",
      attempt_count: 0
    });
    return file;
  }

  async dropWakeJobsForRoom(roomId) {
    const targetRoomID = String(roomId || "").trim();
    if (!targetRoomID) {
      return;
    }
    const files = await listJSONFiles(this.paths.wakeQueueDir);
    for (const file of files) {
      const job = await readJSON(file, null);
      if (!job) {
        continue;
      }
      if (String(job.room_id || "").trim() !== targetRoomID) {
        continue;
      }
      await removeIfExists(file);
    }
  }

  async updateWakeJob(jobFile, value) {
    await atomicWriteJSON(jobFile, value);
  }

  async ackDelivery(deliveryId) {
    await this.requestJSON("/v1/agent/stream/ack", {
      method: "POST",
      body: { delivery_id: deliveryId }
    });
    this.state.last_acknowledged_delivery_id = deliveryId;
    this.state.last_stream_status = "acked";
    await this.saveState();
  }

  async wakeOpenClaw(job) {
    const roomId = String(job.room_id || "").trim();
    const deliveryId = String(job.delivery_id || "").trim();
    const hookURL = normalizeURL(this.config.openclaw.hook_url);
    const hookToken = String(this.config.openclaw.hook_token || "").trim();
    if (!hookURL || !hookToken) {
      throw new Error("OpenClaw hook_url/hook_token missing from bridge config");
    }
    const tokenState = await this.ensureFreshRoomTokenForJob(job);
    const tokenExpiresAt = tokenState?.expires_at || job.token_expires_at || null;
    const tokenPath = path.join(this.paths.tokenDir, `${roomId}.json`);
    const typingToken = String(tokenState?.token || job.room_token || "").trim();
    let typingStarted = false;
    if (typingToken) {
      typingStarted = await this.sendRoomTypingSignal(roomId, typingToken, "start", DEFAULT_TYPING_TTL_MS);
    }
    const contract = {
      contract: "aya.wake.v1",
      delivery_id: deliveryId,
      event_type: String(job.type || "room.turn_ready"),
      event_id: job.event_id || null,
      reason: job.reason || null,
      room_id: roomId,
      room_state: job.room_state || null,
      next_turn: typeof job.next_turn === "number" ? job.next_turn : null,
      next_actor_id: job.next_actor_id || null,
      occurred_at: job.occurred_at || null,
      token_path: tokenPath,
      token_expires_at: tokenExpiresAt,
      instructions: [
        "Read token_path, fetch fresh /v1/rooms/{id}/context, then POST /v1/rooms/{id}/context/ack with the returned turn_index after the response parses successfully.",
        "Reply exactly once only if next_actor_id in fresh context equals your agent_id.",
        "If token missing/expired or API returns 401, refresh with POST /v1/rooms/{id}/access-token and retry once.",
        "If API returns 409 turn_mismatch or stale_bundle_hash, stop and wait for next wake."
      ]
    };
    const message = [
      "[AYA_WAKE_V1]",
      JSON.stringify(contract)
    ].join("\n");
    const payload = {
      agentId: this.config.openclaw.agent_id || "main",
      message,
      name: "areyouai",
      wakeMode: "now",
      deliver: false,
      timeoutSeconds: 120
    };
    try {
      const response = await this.fetch(hookURL, {
        method: "POST",
        headers: {
          Authorization: `Bearer ${hookToken}`,
          "Content-Type": "application/json"
        },
        body: JSON.stringify(payload)
      });
      const text = await response.text();
      if (!response.ok) {
        throw new HTTPError(response.status, parseJSONText(text), `OpenClaw wake failed ${response.status}`);
      }
      return parseJSONText(text);
    } finally {
      if (typingStarted) {
        await this.sendRoomTypingSignal(roomId, typingToken, "stop");
      }
    }
  }

  async drainWakeQueue() {
    const files = await listJSONFiles(this.paths.wakeQueueDir);
    for (const file of files) {
      const job = await readJSON(file, null);
      if (!job) {
        continue;
      }
      try {
        await this.wakeOpenClaw(job);
        await removeIfExists(file);
        const completedKey = this.normalizeTurnReadyWakeKey(job);
        if (completedKey) {
          this.rememberCompletedWakeKey(completedKey);
          try {
            await this.saveState();
          } catch (err) {
            this.logger.warn(`wake state persist failed delivery_id=${job.delivery_id} room_id=${job.room_id} error=${err.message}`);
          }
        }
        this.logger.info(`wake delivered delivery_id=${job.delivery_id} room_id=${job.room_id}`);
      } catch (err) {
        const nextJob = {
          ...job,
          attempt_count: Number(job.attempt_count || 0) + 1,
          last_error: err.message,
          updated_at: new Date().toISOString()
        };
        await this.updateWakeJob(file, nextJob);
        this.logger.warn(`wake failed delivery_id=${job.delivery_id} room_id=${job.room_id} error=${err.message}`);
      }
    }
  }

  async processRecovery() {
    this.state.last_stream_status = "recovery";
    await this.saveState();
    const payload = await this.requestJSON("/v1/agent/actionable-rooms");
    for (const item of payload.terminal || []) {
      if (item.room_id) {
        await this.deleteRoomToken(item.room_id);
        await this.dropWakeJobsForRoom(item.room_id);
        this.forgetCompletedWakeKeysForRoom(item.room_id);
        await this.saveState();
      }
    }
    for (const item of payload.actionable || []) {
      const roomId = String(item.room_id || "").trim();
      const token = String(item.token || item.room_token || "").trim();
      if (!roomId || !token) {
        continue;
      }
      const wakeKey = this.normalizeTurnReadyWakeKey(item);
      if (wakeKey && this.hasCompletedWakeKey(wakeKey)) {
        await this.dropWakeJobsForRoom(roomId);
        continue;
      }
      // Recovery payload is authoritative; clear stale queued wake jobs for this room.
      await this.dropWakeJobsForRoom(roomId);
      await this.writeRoomToken(roomId, item);
      const syntheticDeliveryID = `recovery-${roomId}-${item.next_turn ?? "unknown"}`;
      await this.enqueueWakeJob({
        delivery_id: syntheticDeliveryID,
        type: "room.turn_ready",
        room_id: roomId,
        reason: "replay_recovery",
        room_state: item.room_state || "ACTIVE",
        next_turn: typeof item.next_turn === "number" ? item.next_turn : null,
        next_actor_id: item.next_actor_id || null,
        occurred_at: new Date().toISOString(),
        token_expires_at: item.expires_at || null,
        received_at: new Date().toISOString()
      });
    }
    await this.drainWakeQueue();
    if (String(this.state.last_acknowledged_delivery_id || "").trim()) {
      this.state.last_acknowledged_delivery_id = "";
      await this.saveState();
    }
  }

  async handleTurnReady(payload) {
    const deliveryId = String(payload.delivery_id || "").trim();
    const roomId = String(payload.room_id || "").trim();
    if (!deliveryId || !roomId) {
      throw new Error("room.turn_ready missing delivery_id or room_id");
    }
    const wakeKey = this.normalizeTurnReadyWakeKey(payload);
    if (wakeKey && this.hasCompletedWakeKey(wakeKey)) {
      await this.ackDelivery(deliveryId);
      this.logger.info(`delivery deduped delivery_id=${deliveryId} room_id=${roomId} event_type=room.turn_ready`);
      return payload;
    }
    const token = String(payload.room_token || "").trim();
    let tokenPayload = payload;
    if (!token) {
      tokenPayload = await this.refreshRoomToken(roomId);
    } else {
      await this.writeRoomToken(roomId, payload);
    }
    await this.enqueueWakeJob({
      delivery_id: deliveryId,
      type: payload.type || "room.turn_ready",
      room_id: roomId,
      event_id: payload.event_id || null,
      reason: payload.reason || null,
      room_state: payload.room_state || "ACTIVE",
      next_turn: typeof payload.next_turn === "number" ? payload.next_turn : null,
      next_actor_id: payload.next_actor_id || null,
      occurred_at: payload.occurred_at || new Date().toISOString(),
      token_expires_at: tokenPayload.expires_at || payload.expires_at || null,
      received_at: new Date().toISOString()
    });
    await this.ackDelivery(deliveryId);
    this.logger.info(`delivery acked delivery_id=${deliveryId} room_id=${roomId} event_type=room.turn_ready`);
    await this.drainWakeQueue();
    return tokenPayload;
  }

  async handleTerminal(payload) {
    const deliveryId = String(payload.delivery_id || "").trim();
    const roomId = String(payload.room_id || "").trim();
    if (roomId) {
      await this.deleteRoomToken(roomId);
      await this.dropWakeJobsForRoom(roomId);
      this.forgetCompletedWakeKeysForRoom(roomId);
      await this.saveState();
    }
    if (deliveryId) {
      await this.ackDelivery(deliveryId);
      this.logger.info(`delivery acked delivery_id=${deliveryId} room_id=${roomId} event_type=${payload.type}`);
    }
  }

  async handleStreamEvent(event) {
    const payload = event.data || {};
    switch (event.event) {
      case "stream.hello":
        if (payload.agent_id) {
          this.session.agent_id = payload.agent_id;
          await this.saveSession();
        }
        if (payload.resume_status === "replay_required") {
          await this.processRecovery();
          return "reconnect";
        }
        return "continue";
      case "stream.replay_required":
        await this.processRecovery();
        return "reconnect";
      case "auth.relogin_required":
        await this.relogin();
        return "reconnect";
      case "room.turn_ready":
        await this.handleTurnReady(payload);
        return "continue";
      case "room.closed":
      case "room.purged":
        await this.handleTerminal(payload);
        return "continue";
      default:
        this.logger.debug(`ignoring stream event ${event.event}`);
        return "continue";
    }
  }

  async openStream(signal) {
    const lastDeliveryID = String(this.state.last_acknowledged_delivery_id || "").trim();
    const url = new URL("/v1/agent/stream", `${normalizeURL(this.config.aya.api_base_url)}/`);
    if (lastDeliveryID) {
      url.searchParams.set("last_delivery_id", lastDeliveryID);
    }
    await this.ensureSession();
    let response = await this.fetch(url, {
      method: "GET",
      headers: {
        Authorization: `Bearer ${this.session.session_token}`,
        Accept: "text/event-stream"
      },
      signal
    });
    if (response.status === 401) {
      await this.relogin();
      response = await this.fetch(url, {
        method: "GET",
        headers: {
          Authorization: `Bearer ${this.session.session_token}`,
          Accept: "text/event-stream"
        },
        signal
      });
    }
    if (!response.ok) {
      const text = await response.text();
      throw new HTTPError(response.status, parseJSONText(text), `stream connect failed ${response.status}`);
    }
    const contentType = String(response.headers.get("content-type") || "");
    if (!contentType.startsWith("text/event-stream")) {
      throw new Error(`unexpected stream content-type: ${contentType}`);
    }
    this.state.last_connected_at = new Date().toISOString();
    this.state.last_stream_status = "connected";
    await this.saveState();
    return response;
  }

  async serve(signal) {
    await this.ensureLayout();
    await this.drainWakeQueue();
    let attempt = 0;
    let wakeRetryTimer = null;

    try {
      wakeRetryTimer = setInterval(() => {
        this.drainWakeQueue().catch((err) => {
          this.logger.warn(`wake queue drain failed error=${err.message}`);
        });
      }, Number(this.config.aya.wake_retry_interval_ms || 5000));

      while (!signal?.aborted) {
        try {
          const response = await this.openStream(signal);
          attempt = 0;
          for await (const event of parseSSE(response.body)) {
            const action = await this.handleStreamEvent(event);
            if (action === "reconnect") {
              await response.body?.cancel?.();
              break;
            }
          }
        } catch (err) {
          if (signal?.aborted && err?.name === "AbortError") {
            break;
          }
          this.state.last_stream_status = "disconnected";
          await this.saveState();
          const log = isExpectedStreamDisconnectError(err) ? this.logger.info : this.logger.warn;
          log(`stream disconnected error=${err.message}`);
        }
        if (signal?.aborted) {
          break;
        }
        const delay = computeBackoffMs(this.config.aya.reconnect, attempt);
        attempt += 1;
        await sleep(delay, undefined, { signal }).catch(() => {});
      }
    } finally {
      if (wakeRetryTimer) {
        clearInterval(wakeRetryTimer);
      }
    }
  }
}

module.exports = {
  BridgeDaemon,
  HTTPError,
  DEFAULT_BASE_DIR,
  atomicWriteJSON,
  createLogger,
  defaultConfig,
  expandHome,
  listJSONFiles,
  parseSSE,
  parseSSEFrame,
  readJSON,
  resolvePaths
};

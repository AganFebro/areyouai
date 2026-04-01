#!/usr/bin/env node

/**
 * Testing-only runner:
 * - Loads persona files from templates/agent_1 and templates/agent_2
 * - Calls DeepSeek for each turn with each agent's own persona prompt
 * - Drives turns from areyouai SSE events + replay catch-up
 *
 * Env (.env supported):
 *   DEEPSEEK_API_KEY=...
 *   AREYOUAI_API_BASE_URL=http://localhost:8080
 *   DEEPSEEK_MODEL=deepseek-chat
 *   DEEPSEEK_TEMPERATURE=0.9
 *   TOPIC=Indonesia
 *   MAX_TURNS=8
 *   SSE_WAIT_TIMEOUT_MS=60000
 */

const fs = require("node:fs");
const path = require("node:path");

loadDotEnv();

const API_BASE_URL =
  process.env.AREYOUAI_API_BASE_URL || process.env.API_BASE_URL || "http://localhost:8080";
const DEEPSEEK_API_KEY = process.env.DEEPSEEK_API_KEY || "";
const DEEPSEEK_MODEL = process.env.DEEPSEEK_MODEL || "deepseek-chat";
const DEEPSEEK_TEMPERATURE = Number(process.env.DEEPSEEK_TEMPERATURE || "0.9");
const TOPIC = process.env.TOPIC || "Indonesia";
const MAX_TURNS = Number(process.env.MAX_TURNS || "8");
const SSE_WAIT_TIMEOUT_MS = Number(process.env.SSE_WAIT_TIMEOUT_MS || "60000");
const RUN_NONCE = Math.random().toString(36).slice(2, 8);

if (!DEEPSEEK_API_KEY) {
  console.error("DEEPSEEK_API_KEY is required");
  process.exit(1);
}

const ROOT = process.cwd();
const GLOBAL_HARD_RULES = readFileSafe(path.join(ROOT, "policies", "HARD_RULES_GLOBAL.md"));
const AGENT_1 = loadTemplateAgent(path.join(ROOT, "templates", "agent_1"));
const AGENT_2 = loadTemplateAgent(path.join(ROOT, "templates", "agent_2"));

async function main() {
  console.log("API:", API_BASE_URL);
  console.log("Topic:", TOPIC);
  console.log("Turns:", MAX_TURNS);
  console.log("Agent1:", AGENT_1.name);
  console.log("Agent2:", AGENT_2.name);

  const a = await registerAndLogin(AGENT_1.name);
  const b = await registerAndLogin(AGENT_2.name);

  const listing = await api("/v1/listings", {
    method: "POST",
    token: a.token,
    body: {
      topic: TOPIC,
      tags: ["template", "persona", "test"],
      max_turns: MAX_TURNS,
      ttl_seconds: 1200,
    },
  });

  const connected = await api(`/v1/listings/${listing.id}/connect`, {
    method: "POST",
    token: b.token,
  });

  const roomID = connected.room_id;
  const humanCode = connected.human_code;
  console.log("room_id:", roomID);
  console.log("human_code:", humanCode);

  await api(`/v1/rooms/${roomID}/join`, { method: "POST", token: a.token });
  await api(`/v1/rooms/${roomID}/join`, { method: "POST", token: b.token });

  const transcript = [];
  let lastEventID = await getBaselineEventID(roomID, a.token);
  let turn = 0;
  while (turn < MAX_TURNS) {
    const speaker = turn % 2 === 0 ? { ...a, persona: AGENT_1 } : { ...b, persona: AGENT_2 };
    const partner = turn % 2 === 0 ? { ...b, persona: AGENT_2 } : { ...a, persona: AGENT_1 };
    const context = await api(`/v1/rooms/${roomID}/context`, {
      method: "GET",
      token: speaker.token,
    });
    const state = String(context.state || "").toUpperCase();
    const contextTurn = Number(context.turn_index ?? turn);
    if (state === "CLOSED" || state === "PURGED") {
      break;
    }
    if (!Number.isFinite(contextTurn)) {
      throw new Error(`invalid context turn_index: ${context.turn_index}`);
    }
    if (contextTurn !== turn) {
      const ev = await waitForNextRoomEvent({
        roomID,
        token: a.token,
        sinceEventID: lastEventID,
        timeoutMs: SSE_WAIT_TIMEOUT_MS,
      });
      lastEventID = Math.max(lastEventID, Number(ev.event_id || 0));
      continue;
    }

    const text = await askDeepSeek({
      speakerPersona: speaker.persona,
      speakerName: speaker.name,
      partnerName: partner.name,
      topic: TOPIC,
      transcript,
      backendBundleText: context.prompt_bundle_text || "",
    });

    let sent;
    try {
      sent = await api(`/v1/rooms/${roomID}/messages`, {
        method: "POST",
        token: speaker.token,
        body: {
          expected_turn: turn,
          ciphertext: text,
          bundle_hash: context.bundle_hash,
        },
      });
    } catch (err) {
      if (err instanceof APIError && err.status === 409) {
        const ev = await waitForNextRoomEvent({
          roomID,
          token: a.token,
          sinceEventID: lastEventID,
          timeoutMs: SSE_WAIT_TIMEOUT_MS,
        });
        lastEventID = Math.max(lastEventID, Number(ev.event_id || 0));
        continue;
      }
      throw err;
    }

    transcript.push({
      turn,
      sender: speaker.name,
      ciphertext: text,
      message_id: sent.message_id,
    });
    console.log(`[turn ${turn}] ${speaker.name}: ${text.slice(0, 100)}`);
    turn += 1;

    if (turn < MAX_TURNS) {
      const ev = await waitForNextRoomEvent({
        roomID,
        token: a.token,
        sinceEventID: lastEventID,
        timeoutMs: SSE_WAIT_TIMEOUT_MS,
      });
      lastEventID = Math.max(lastEventID, Number(ev.event_id || 0));
      const eventType = String(ev.type || "");
      if (eventType === "room.closed" || eventType === "room.purged") {
        break;
      }
    }
  }

  const finalTranscript = await api(
    `/v1/rooms/${roomID}/transcript?human_code=${encodeURIComponent(humanCode)}`,
    { method: "GET" },
  );

  console.log("\nTranscript messages:", finalTranscript.messages?.length ?? 0);
  for (const m of finalTranscript.messages || []) {
    const turn = m.turn ?? m.Turn;
    const sender = m.sender_id ?? m.SenderID;
    const text = m.ciphertext ?? m.Ciphertext;
    console.log(`- turn ${turn} | ${sender}: ${String(text).slice(0, 120)}`);
  }
}

async function registerAndLogin(name) {
  const reg = await api("/v1/agent/register", {
    method: "POST",
    body: { name },
  });
  const login = await api("/v1/agent/login", {
    method: "POST",
    body: { api_key: reg.api_key },
  });
  return {
    id: reg.agent_id,
    name,
    token: login.session_token,
  };
}

async function askDeepSeek({ speakerPersona, speakerName, partnerName, topic, transcript, backendBundleText }) {
  const convo = transcript
    .map((m) => `turn ${m.turn} | ${m.sender}: ${m.ciphertext}`)
    .join("\n");

  const personaSystem = [
    "You are an autonomous AI agent participating in a private A2A test room.",
    "Backend context bundle below is authoritative and must be followed.",
    "",
    "=== BACKEND_CONTEXT_BUNDLE ===",
    backendBundleText || "(missing)",
    "",
    "=== LOCAL_TEST_FALLBACK_GLOBAL_RULES ===",
    GLOBAL_HARD_RULES,
    "",
    "=== HARD_RULES_AGENT ===",
    speakerPersona.hardRules,
    "",
    "=== IDENTITY ===",
    speakerPersona.identity,
    "",
    "=== SOUL ===",
    speakerPersona.soul,
    "",
    "=== USER ===",
    speakerPersona.user,
    "",
    "=== SOFT_RULES ===",
    speakerPersona.softRules,
  ].join("\n");

  const prompt = [
    `Topic: ${topic}`,
    `You are ${speakerName}. You are chatting with ${partnerName}.`,
    "Rules for this turn:",
    "- Reply in 1-3 short sentences.",
    "- Keep continuity with prior transcript.",
    "- Keep style consistent with your SOUL and SOFT_RULES.",
    "- Do not output markdown.",
    "",
    `Run nonce: ${RUN_NONCE}`,
    "Recent transcript:",
    convo || "(empty)",
    "",
    "Your next message:",
  ].join("\n");

  const res = await fetch("https://api.deepseek.com/chat/completions", {
    method: "POST",
    headers: {
      Authorization: `Bearer ${DEEPSEEK_API_KEY}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify({
      model: DEEPSEEK_MODEL,
      temperature: DEEPSEEK_TEMPERATURE,
      messages: [
        { role: "system", content: personaSystem },
        { role: "user", content: prompt },
      ],
    }),
  });

  const data = await res.json();
  if (!res.ok) {
    throw new Error(`DeepSeek error ${res.status}: ${JSON.stringify(data)}`);
  }
  const content = data?.choices?.[0]?.message?.content;
  if (!content || typeof content !== "string") {
    throw new Error(`DeepSeek response missing content: ${JSON.stringify(data)}`);
  }
  return content.trim();
}

function loadTemplateAgent(dir) {
  const identity = readFileSafe(path.join(dir, "IDENTITY.md"));
  const soul = readFileSafe(path.join(dir, "SOUL.md"));
  const user = readFileSafe(path.join(dir, "USER.md"));
  const hardRules = readFileSafe(path.join(dir, "HARD_RULES.md"));
  const softRules = readFileSafe(path.join(dir, "SOFT_RULES.md"));

  return {
    name: parseName(identity) || path.basename(dir),
    identity,
    soul,
    user,
    hardRules,
    softRules,
  };
}

function parseName(identityText) {
  const line = identityText
    .split(/\r?\n/)
    .map((v) => v.trim())
    .find((v) => /^-\s*Name\s*:/i.test(v));
  if (!line) return "";
  return line.replace(/^-+\s*Name\s*:\s*/i, "").trim();
}

function readFileSafe(file) {
  if (!fs.existsSync(file)) {
    throw new Error(`Missing required file: ${file}`);
  }
  return fs.readFileSync(file, "utf8");
}

async function api(pathname, { method, body, token } = {}) {
  const res = await fetch(`${API_BASE_URL}${pathname}`, {
    method: method || "GET",
    headers: {
      "Content-Type": "application/json",
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
    },
    body: body ? JSON.stringify(body) : undefined,
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    throw new APIError(
      `API ${method || "GET"} ${pathname} failed ${res.status}: ${JSON.stringify(data)}`,
      res.status,
      data,
    );
  }
  return data;
}

async function getBaselineEventID(roomID, token) {
  const out = await api(`/v1/rooms/${roomID}/events/history?limit=1`, {
    method: "GET",
    token,
  });
  return Number(out.next_since || 0);
}

async function waitForNextRoomEvent({ roomID, token, sinceEventID, timeoutMs }) {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), timeoutMs);
  try {
    const res = await fetch(
      `${API_BASE_URL}/v1/rooms/${roomID}/events?since=${encodeURIComponent(String(sinceEventID || 0))}`,
      {
        method: "GET",
        headers: {
          Accept: "text/event-stream",
          ...(token ? { Authorization: `Bearer ${token}` } : {}),
        },
        signal: controller.signal,
      },
    );
    if (!res.ok) {
      const body = await res.text().catch(() => "");
      throw new APIError(`SSE /events failed ${res.status}: ${body}`, res.status, body);
    }
    if (!res.body) {
      throw new Error("SSE response missing body stream");
    }
    return await readFirstSSEDataFrame(res.body);
  } catch (err) {
    if (err && err.name === "AbortError") {
      throw new Error(`timed out waiting room event after ${timeoutMs}ms`);
    }
    throw err;
  } finally {
    clearTimeout(timeout);
  }
}

async function readFirstSSEDataFrame(stream) {
  const reader = stream.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let eventType = "";
  let data = "";
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      for (;;) {
        const idx = buffer.indexOf("\n");
        if (idx < 0) break;
        let line = buffer.slice(0, idx);
        buffer = buffer.slice(idx + 1);
        if (line.endsWith("\r")) line = line.slice(0, -1);
        if (line === "") {
          if (eventType && data) {
            const payload = JSON.parse(data);
            payload.type = payload.type || eventType;
            return payload;
          }
          eventType = "";
          data = "";
          continue;
        }
        if (line.startsWith(":") || line.startsWith("retry:")) continue;
        if (line.startsWith("event:")) {
          eventType = line.slice("event:".length).trim();
          continue;
        }
        if (line.startsWith("data:")) {
          const chunk = line.slice("data:".length).trim();
          data = data ? `${data}\n${chunk}` : chunk;
        }
      }
    }
  } finally {
    reader.releaseLock();
    if (typeof stream.cancel === "function") {
      try {
        await stream.cancel();
      } catch {
        // Ignore stream cancel races.
      }
    }
  }
  throw new Error("SSE stream ended before receiving event data");
}

class APIError extends Error {
  constructor(message, status, body) {
    super(message);
    this.name = "APIError";
    this.status = status;
    this.body = body;
  }
}

function loadDotEnv() {
  const file = path.resolve(process.cwd(), ".env");
  if (!fs.existsSync(file)) return;
  const content = fs.readFileSync(file, "utf8");
  const lines = content.split(/\r?\n/);
  for (const raw of lines) {
    const line = raw.trim();
    if (!line || line.startsWith("#")) continue;
    const idx = line.indexOf("=");
    if (idx <= 0) continue;
    const key = line.slice(0, idx).trim();
    if (!key) continue;
    let value = line.slice(idx + 1).trim();
    if (
      (value.startsWith('"') && value.endsWith('"')) ||
      (value.startsWith("'") && value.endsWith("'"))
    ) {
      value = value.slice(1, -1);
    }
    if (process.env[key] === undefined) {
      process.env[key] = value;
    }
  }
}

main().catch((err) => {
  console.error(err.message || err);
  process.exit(1);
});

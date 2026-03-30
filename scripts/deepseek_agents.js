#!/usr/bin/env node

/**
 * Simulate 2 agents using DeepSeek API against areyouai backend.
 *
 * Required:
 *   DEEPSEEK_API_KEY=...
 *
 * Optional:
 *   AREYOUAI_API_BASE_URL=http://localhost:8080
 *   API_BASE_URL=http://localhost:8080 (legacy fallback)
 *   DEEPSEEK_MODEL=deepseek-chat
 *   TOPIC="AI safety debate"
 *   MAX_TURNS=6
 */

const fs = require("node:fs");
const path = require("node:path");

loadDotEnv();

const API_BASE_URL =
  process.env.AREYOUAI_API_BASE_URL || process.env.API_BASE_URL || "http://localhost:8080";
const DEEPSEEK_API_KEY = process.env.DEEPSEEK_API_KEY || "";
const DEEPSEEK_MODEL = process.env.DEEPSEEK_MODEL || "deepseek-chat";
const DEEPSEEK_TEMPERATURE = Number(process.env.DEEPSEEK_TEMPERATURE || "0.9");
const TOPIC = process.env.TOPIC || "Should agents disclose uncertainty?";
const MAX_TURNS = Number(process.env.MAX_TURNS || "6");
const RUN_NONCE = Math.random().toString(36).slice(2, 8);

if (!DEEPSEEK_API_KEY) {
  console.error("DEEPSEEK_API_KEY is required");
  process.exit(1);
}

async function main() {
  console.log("API:", API_BASE_URL);
  console.log("Topic:", TOPIC);
  console.log("Turns:", MAX_TURNS);

  const a = await registerAndLogin("deepseek-agent-a");
  const b = await registerAndLogin("deepseek-agent-b");

  const listing = await api("/v1/listings", {
    method: "POST",
    token: a.token,
    body: {
      topic: TOPIC,
      tags: ["deepseek", "auto"],
      max_turns: MAX_TURNS,
      ttl_seconds: 900,
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

  for (let turn = 0; turn < MAX_TURNS; turn += 1) {
    const speaker = turn % 2 === 0 ? a : b;
    const partner = turn % 2 === 0 ? b : a;

    const text = await askDeepSeek({
      speakerName: speaker.name,
      partnerName: partner.name,
      topic: TOPIC,
      transcript,
    });

    const sent = await api(`/v1/rooms/${roomID}/messages`, {
      method: "POST",
      token: speaker.token,
      body: {
        expected_turn: turn,
        ciphertext: text,
      },
    });

    transcript.push({
      turn,
      sender: speaker.name,
      ciphertext: text,
      message_id: sent.message_id,
    });
    console.log(`[turn ${turn}] ${speaker.name}: ${text.slice(0, 90)}`);
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

async function askDeepSeek({ speakerName, partnerName, topic, transcript }) {
  const convo = transcript
    .map((m) => `turn ${m.turn} | ${m.sender}: ${m.ciphertext}`)
    .join("\n");

  const prompt = [
    `Topic: ${topic}`,
    `You are ${speakerName}.`,
    `You are talking to ${partnerName} in a concise 1:1 agent chat.`,
    "Rules:",
    "- 1 to 2 short sentences.",
    "- Stay on topic.",
    "- No markdown, no labels, plain text only.",
    "",
    "Recent transcript:",
    `Run nonce: ${RUN_NONCE}`,
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
        { role: "system", content: "You are an autonomous agent in a private agent-to-agent room." },
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

async function api(path, { method, body, token } = {}) {
  const res = await fetch(`${API_BASE_URL}${path}`, {
    method: method || "GET",
    headers: {
      "Content-Type": "application/json",
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
    },
    body: body ? JSON.stringify(body) : undefined,
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    throw new Error(`API ${method || "GET"} ${path} failed ${res.status}: ${JSON.stringify(data)}`);
  }
  return data;
}

main().catch((err) => {
  console.error(err.message || err);
  process.exit(1);
});

function loadDotEnv() {
  const file = path.resolve(process.cwd(), ".env");
  if (!fs.existsSync(file)) return;

  const content = fs.readFileSync(file, "utf8");
  const lines = content.split(/\r?\n/);
  for (const rawLine of lines) {
    const line = rawLine.trim();
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

    // Environment passed at runtime wins over .env values.
    if (process.env[key] === undefined) {
      process.env[key] = value;
    }
  }
}

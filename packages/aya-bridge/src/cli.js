"use strict";

const fs = require("node:fs/promises");
const { constants: fsConstants } = require("node:fs");
const path = require("node:path");
const readline = require("node:readline/promises");
const { stdin, stdout } = require("node:process");

const {
  BridgeDaemon,
  createLogger,
  defaultConfig,
  expandHome,
  pathExists,
  readJSON,
  resolvePaths
} = (() => {
  const mod = require("./bridge");
  return {
    ...mod,
    pathExists: async (target) => {
      try {
        await fs.access(target);
        return true;
      } catch {
        return false;
      }
    },
    pathWritable: async (target) => {
      try {
        await fs.access(target, fsConstants.W_OK);
        return true;
      } catch {
        return false;
      }
    }
  };
})();

function parseArgs(argv) {
  const args = { _: [] };
  for (let i = 0; i < argv.length; i += 1) {
    const part = argv[i];
    if (!part.startsWith("--")) {
      args._.push(part);
      continue;
    }
    const eqIdx = part.indexOf("=");
    if (eqIdx > -1) {
      args[part.slice(2, eqIdx)] = part.slice(eqIdx + 1);
      continue;
    }
    const key = part.slice(2);
    const next = argv[i + 1];
    if (!next || next.startsWith("--")) {
      args[key] = true;
      continue;
    }
    args[key] = next;
    i += 1;
  }
  return args;
}

async function promptInput(rl, label, defaultValue = "") {
  const suffix = defaultValue ? ` [${defaultValue}]` : "";
  const answer = await rl.question(`${label}${suffix}: `);
  const trimmed = answer.trim();
  return trimmed || defaultValue;
}

async function commandInit(args) {
  const baseDir = expandHome(String(args["base-dir"] || "").trim() || undefined);
  const config = defaultConfig(baseDir);
  const paths = resolvePaths(config);
  const exists = await pathExists(paths.configPath);
  if (exists && !args.force) {
    console.log(JSON.stringify({ ok: true, config_path: paths.configPath, reused: true }, null, 2));
    return;
  }

  const rl = args["non-interactive"] ? null : readline.createInterface({ input: stdin, output: stdout });
  try {
    config.aya.api_base_url = normalizeArg(
      args["aya-api-base-url"],
      rl ? await promptInput(rl, "AYA API base URL", config.aya.api_base_url) : config.aya.api_base_url
    );
    config.openclaw.hook_url = normalizeArg(
      args["openclaw-hook-url"],
      rl ? await promptInput(rl, "OpenClaw hook URL", config.openclaw.hook_url) : config.openclaw.hook_url
    );
    config.openclaw.hook_token = normalizeArg(
      args["openclaw-hook-token"],
      rl ? await promptInput(rl, "OpenClaw hook token", config.openclaw.hook_token) : config.openclaw.hook_token
    );
    config.openclaw.agent_id = normalizeArg(
      args["openclaw-agent-id"],
      rl ? await promptInput(rl, "OpenClaw agent ID", config.openclaw.agent_id) : config.openclaw.agent_id
    );
  } finally {
    await rl?.close();
  }

  const daemon = new BridgeDaemon({ config, logger: createLogger("info") });
  await daemon.ensureLayout();
  await daemon.saveConfig();
  console.log(JSON.stringify({ ok: true, config_path: daemon.paths.configPath }, null, 2));
}

async function commandLogin(args) {
  const daemon = await BridgeDaemon.fromDisk({ logLevel: "info" });
  let apiKey = String(args["api-key"] || "").trim();
  if (args.stdin) {
    apiKey = (await readStdin()).trim();
  }
  if (!apiKey) {
    const rl = readline.createInterface({ input: stdin, output: stdout });
    try {
      apiKey = await promptInput(rl, "AYA API key", "");
    } finally {
      await rl.close();
    }
  }
  if (!apiKey) {
    throw Object.assign(new Error("AYA API key is required"), { exitCode: 3 });
  }
  daemon.session = {
    ...daemon.session,
    api_key: apiKey
  };
  await daemon.relogin();
  console.log(JSON.stringify({ ok: true, session_path: daemon.paths.sessionPath }, null, 2));
}

async function commandServe(args) {
  const logger = createLogger(String(args["log-level"] || "info"));
  const daemon = await BridgeDaemon.fromDisk({ logLevel: String(args["log-level"] || "info"), logger });
  const controller = new AbortController();
  process.on("SIGINT", () => controller.abort());
  process.on("SIGTERM", () => controller.abort());
  await daemon.serve(controller.signal);
}

async function commandStatus() {
  const daemon = await BridgeDaemon.fromDisk({ logLevel: "error" });
  const tokenFiles = await fs.readdir(daemon.paths.tokenDir).catch(() => []);
  const wakeFiles = await fs.readdir(daemon.paths.wakeQueueDir).catch(() => []);
  const output = {
    version: "0.1.0",
    api_base_url: daemon.config.aya.api_base_url,
    hook_url: daemon.config.openclaw.hook_url,
    agent_id: daemon.session.agent_id || daemon.config.openclaw.agent_id || "",
    has_session: Boolean(daemon.session.session_token),
    last_acknowledged_delivery_id: daemon.state.last_acknowledged_delivery_id || "",
    last_connected_at: daemon.state.last_connected_at || null,
    last_stream_status: daemon.state.last_stream_status || "idle",
    token_file_count: tokenFiles.filter((file) => file.endsWith(".json")).length,
    wake_queue_count: wakeFiles.filter((file) => file.endsWith(".json")).length
  };
  console.log(JSON.stringify(output, null, 2));
}

async function commandLogout() {
  const config = await readJSON(path.join(expandHome("~/.areyouai"), "config.json"), null);
  if (!config) {
    throw Object.assign(new Error("bridge config not found; run init first"), { exitCode: 2 });
  }
  const daemon = await BridgeDaemon.fromDisk({ logLevel: "error" });
  await fs.rm(daemon.paths.sessionPath, { force: true });
  console.log(JSON.stringify({ ok: true, session_path: daemon.paths.sessionPath, removed: true }, null, 2));
}

async function commandDoctor() {
  const daemon = await BridgeDaemon.fromDisk({ logLevel: "error" });
  const checks = {
    config_exists: await pathExists(daemon.paths.configPath),
    session_exists: await pathExists(daemon.paths.sessionPath),
    token_dir_exists: await pathExists(daemon.paths.tokenDir),
    wake_queue_dir_exists: await pathExists(daemon.paths.wakeQueueDir),
    token_dir_writable: await pathWritable(daemon.paths.tokenDir),
    wake_queue_dir_writable: await pathWritable(daemon.paths.wakeQueueDir),
    openclaw_hook_configured: Boolean(String(daemon.config.openclaw.hook_url || "").trim()) && Boolean(String(daemon.config.openclaw.hook_token || "").trim()),
    api_health: false
  };
  try {
    const response = await daemon.fetch(new URL("/healthz", `${daemon.config.aya.api_base_url}/`));
    checks.api_health = response.ok;
  } catch {
    checks.api_health = false;
  }
  console.log(JSON.stringify(checks, null, 2));
  if (!checks.config_exists || !checks.api_health || !checks.openclaw_hook_configured || !checks.token_dir_writable || !checks.wake_queue_dir_writable) {
    throw Object.assign(new Error("doctor failed"), { exitCode: 2 });
  }
}

function normalizeArg(flagValue, promptedValue) {
  const raw = String(flagValue || promptedValue || "").trim();
  return raw.replace(/\/+$/, "");
}

async function readStdin() {
  const chunks = [];
  for await (const chunk of stdin) {
    chunks.push(chunk);
  }
  return Buffer.concat(chunks).toString("utf8");
}

async function main(argv) {
  const args = parseArgs(argv);
  const [command] = args._;
  switch (command) {
    case "init":
      return commandInit(args);
    case "login":
      return commandLogin(args);
    case "serve":
      return commandServe(args);
    case "status":
      return commandStatus(args);
    case "logout":
      return commandLogout(args);
    case "doctor":
      return commandDoctor(args);
    default:
      console.log("usage: aya <init|login|serve|status|logout|doctor> [--flags]");
      return;
  }
}

module.exports = {
  main,
  parseArgs
};

#!/usr/bin/env node

const { main } = require("../src/cli");

main(process.argv.slice(2)).catch((err) => {
  const message = err && err.message ? err.message : String(err);
  console.error(message);
  process.exit(typeof err?.exitCode === "number" ? err.exitCode : 1);
});

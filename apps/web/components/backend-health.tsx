"use client";

import { useEffect, useState } from "react";
import { config } from "@/lib/config";

type Status = "idle" | "loading" | "ok" | "error";

export function BackendHealth() {
  const [status, setStatus] = useState<Status>("idle");

  useEffect(() => {
    let mounted = true;
    const check = async () => {
      setStatus("loading");
      try {
        const res = await fetch(`${config.apiBaseUrl}/healthz`, { cache: "no-store" });
        if (!mounted) return;
        setStatus(res.ok ? "ok" : "error");
      } catch {
        if (!mounted) return;
        setStatus("error");
      }
    };
    void check();

    return () => {
      mounted = false;
    };
  }, []);

  return (
    <div
      style={{
        border: "1px solid #334155",
        borderRadius: 12,
        padding: 16,
        background: "#0b1220",
      }}
    >
      <h2 style={{ margin: 0, marginBottom: 8 }}>Backend</h2>
      <p style={{ margin: 0, color: "#94a3b8" }}>API: {config.apiBaseUrl}</p>
      <p style={{ marginTop: 8, marginBottom: 0 }}>
        Health:{" "}
        <strong>
          {status === "idle" && "idle"}
          {status === "loading" && "checking..."}
          {status === "ok" && "ok"}
          {status === "error" && "unreachable"}
        </strong>
      </p>
    </div>
  );
}

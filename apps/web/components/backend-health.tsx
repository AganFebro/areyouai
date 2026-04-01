"use client";

import { useEffect, useState } from "react";
import { IconHeartbeat } from "@tabler/icons-react";
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
    <section className="wb-card health-panel">
      <div className="health-title">
        <IconHeartbeat size={15} />
        <span>Backend Link</span>
      </div>
      <div className="health-row">
        <span className="health-label">API</span>
        <code>{config.apiBaseUrl}</code>
      </div>
      <div className="health-row">
        <span className="health-label">Health</span>
        <strong className={status === "ok" ? "health-ok" : status === "error" ? "health-bad" : "health-muted"}>
          {status === "idle" && "idle"}
          {status === "loading" && "checking..."}
          {status === "ok" && "ok"}
          {status === "error" && "unreachable"}
        </strong>
      </div>
    </section>
  );
}

"use client";

import { type CSSProperties, useEffect, useMemo, useState } from "react";
import { config } from "@/lib/config";

type Overview = {
  agents_total: number;
  sessions_active: number;
  rooms_open: number;
  rooms_active: number;
  rooms_closed: number;
  rooms_purged: number;
  messages_total: number;
  generated_at_utc: string;
};

type AdminRoom = {
  id: string;
  agent_a_id: string;
  agent_a_name: string;
  agent_b_id: string;
  agent_b_name: string;
  state: string;
  turn_index: number;
  max_turns: number;
  ttl_at: string;
  created_at: string;
  closed_at?: string;
  purged_at?: string;
};

type AuditEvent = {
  id: number;
  room_id: string;
  event: string;
  meta: string;
  message_count: number;
  created_at: string;
};

type Status = "idle" | "loading" | "ok" | "error";
const ADMIN_TOKEN_STORAGE_KEY = "areyouai.admin.token";

export function AdminDashboard() {
  const [status, setStatus] = useState<Status>("idle");
  const [statusMessage, setStatusMessage] = useState("idle");
  const [adminToken, setAdminToken] = useState(() => {
    if (typeof window === "undefined") return "";
    return window.localStorage.getItem(ADMIN_TOKEN_STORAGE_KEY) ?? "";
  });
  const [overview, setOverview] = useState<Overview | null>(null);
  const [rooms, setRooms] = useState<AdminRoom[]>([]);
  const [audit, setAudit] = useState<AuditEvent[]>([]);

  useEffect(() => {
    const token = adminToken.trim();
    if (!token) return;

    let mounted = true;
    const load = async () => {
      setStatus("loading");
      setStatusMessage("loading...");
      try {
        const headers = { Authorization: `Bearer ${token}` };
        const [ovRes, roomsRes, auditRes] = await Promise.all([
          fetch(`${config.apiBaseUrl}/v1/admin/overview`, { cache: "no-store", headers }),
          fetch(`${config.apiBaseUrl}/v1/admin/rooms`, { cache: "no-store", headers }),
          fetch(`${config.apiBaseUrl}/v1/admin/audit`, { cache: "no-store", headers }),
        ]);
        if (!mounted) return;
        if (ovRes.status === 401 || roomsRes.status === 401 || auditRes.status === 401) {
          setStatus("error");
          setStatusMessage("unauthorized: invalid admin token");
          return;
        }
        if (!ovRes.ok || !roomsRes.ok || !auditRes.ok) {
          setStatus("error");
          setStatusMessage("admin API error");
          return;
        }
        const ov = (await ovRes.json()) as Overview;
        const r = (await roomsRes.json()) as { items?: AdminRoom[] };
        const a = (await auditRes.json()) as { items?: AuditEvent[] };
        setOverview(ov);
        setRooms(r.items ?? []);
        setAudit(a.items ?? []);
        setStatus("ok");
        setStatusMessage("ok");
      } catch {
        if (!mounted) return;
        setStatus("error");
        setStatusMessage("network error");
      }
    };

    void load();
    const timer = window.setInterval(() => void load(), 10000);
    return () => {
      mounted = false;
      window.clearInterval(timer);
    };
  }, [adminToken]);

  const roomSummary = useMemo(() => {
    const byState = new Map<string, number>();
    for (const room of rooms) {
      byState.set(room.state, (byState.get(room.state) ?? 0) + 1);
    }
    return Array.from(byState.entries());
  }, [rooms]);

  const saveToken = () => {
    const token = adminToken.trim();
    if (!token) {
      setStatusMessage("admin token is required");
      return;
    }
    window.localStorage.setItem(ADMIN_TOKEN_STORAGE_KEY, token);
    setStatusMessage("admin token saved");
  };

  const clearToken = () => {
    window.localStorage.removeItem(ADMIN_TOKEN_STORAGE_KEY);
    setAdminToken("");
    setStatus("idle");
    setStatusMessage("admin token cleared");
    setOverview(null);
    setRooms([]);
    setAudit([]);
  };

  const tokenMissing = adminToken.trim() === "";
  const effectiveStatusMessage = tokenMissing ? "missing admin token" : statusMessage;

  return (
    <section>
      <div
        style={{
          border: "1px solid #334155",
          borderRadius: 12,
          padding: 16,
          background: "#0b1220",
          marginBottom: 16,
        }}
      >
        <div style={{ display: "flex", gap: 8, flexWrap: "wrap", marginBottom: 10 }}>
          <input
            type="password"
            value={adminToken}
            onChange={(e) => setAdminToken(e.target.value)}
            placeholder="ADMIN_TOKEN"
            style={{
              minWidth: 300,
              borderRadius: 8,
              border: "1px solid #334155",
              background: "#020617",
              color: "#e5e7eb",
              padding: "8px 10px",
            }}
          />
          <button
            onClick={saveToken}
            style={{ borderRadius: 8, border: "1px solid #334155", background: "#0f172a", color: "#e5e7eb", padding: "8px 12px", cursor: "pointer" }}
          >
            Save Token
          </button>
          <button
            onClick={clearToken}
            style={{ borderRadius: 8, border: "1px solid #334155", background: "#0f172a", color: "#e5e7eb", padding: "8px 12px", cursor: "pointer" }}
          >
            Clear
          </button>
        </div>
        <strong>API status: </strong>
        {status === "loading" ? "loading..." : status === "ok" ? "ok" : status} (
        {effectiveStatusMessage})
      </div>

      <div
        style={{
          display: "grid",
          gridTemplateColumns: "repeat(auto-fit, minmax(180px, 1fr))",
          gap: 12,
          marginBottom: 20,
        }}
      >
        <StatCard label="Agents" value={overview?.agents_total ?? 0} />
        <StatCard label="Active Sessions" value={overview?.sessions_active ?? 0} />
        <StatCard label="Messages" value={overview?.messages_total ?? 0} />
        <StatCard label="Rooms Open" value={overview?.rooms_open ?? 0} />
        <StatCard label="Rooms Active" value={overview?.rooms_active ?? 0} />
        <StatCard label="Rooms Closed" value={overview?.rooms_closed ?? 0} />
        <StatCard label="Rooms Purged" value={overview?.rooms_purged ?? 0} />
      </div>

      <section style={{ marginBottom: 24 }}>
        <h2 style={{ marginBottom: 8 }}>Rooms</h2>
        <p style={{ marginTop: 0, color: "#94a3b8" }}>
          {roomSummary.map(([state, count]) => `${state}: ${count}`).join(" | ") || "No rooms"}
        </p>
        <div style={{ overflowX: "auto", border: "1px solid #334155", borderRadius: 12 }}>
          <table style={{ width: "100%", borderCollapse: "collapse" }}>
            <thead>
              <tr>
                {["room_id", "state", "turn", "agent_a", "agent_b", "created_at"].map((h) => (
                  <th key={h} style={thStyle}>
                    {h}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {rooms.map((room) => (
                <tr key={room.id}>
                  <td style={tdStyle}>{room.id}</td>
                  <td style={tdStyle}>{room.state}</td>
                  <td style={tdStyle}>
                    {room.turn_index}/{room.max_turns}
                  </td>
                  <td style={tdStyle}>{room.agent_a_name || room.agent_a_id}</td>
                  <td style={tdStyle}>{room.agent_b_name || room.agent_b_id}</td>
                  <td style={tdStyle}>{new Date(room.created_at).toLocaleString()}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>

      <section>
        <h2 style={{ marginBottom: 8 }}>Audit</h2>
        <div style={{ overflowX: "auto", border: "1px solid #334155", borderRadius: 12 }}>
          <table style={{ width: "100%", borderCollapse: "collapse" }}>
            <thead>
              <tr>
                {["id", "event", "room_id", "message_count", "created_at"].map((h) => (
                  <th key={h} style={thStyle}>
                    {h}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {audit.map((ev) => (
                <tr key={ev.id}>
                  <td style={tdStyle}>{ev.id}</td>
                  <td style={tdStyle}>{ev.event}</td>
                  <td style={tdStyle}>{ev.room_id}</td>
                  <td style={tdStyle}>{ev.message_count}</td>
                  <td style={tdStyle}>{new Date(ev.created_at).toLocaleString()}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>
    </section>
  );
}

function StatCard({ label, value }: { label: string; value: number }) {
  return (
    <div
      style={{
        border: "1px solid #334155",
        borderRadius: 12,
        padding: 12,
        background: "#0b1220",
      }}
    >
      <div style={{ color: "#94a3b8", fontSize: 13 }}>{label}</div>
      <div style={{ fontSize: 24, fontWeight: 700 }}>{value}</div>
    </div>
  );
}

const thStyle: CSSProperties = {
  textAlign: "left",
  padding: "10px 12px",
  borderBottom: "1px solid #334155",
  color: "#94a3b8",
  fontSize: 13,
};

const tdStyle: CSSProperties = {
  padding: "10px 12px",
  borderBottom: "1px solid #1e293b",
  fontSize: 14,
};

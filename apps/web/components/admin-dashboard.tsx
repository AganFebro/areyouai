"use client";

import { useEffect, useMemo, useState, type ReactNode } from "react";
import {
  IconActivityHeartbeat,
  IconBrandSpeedtest,
  IconMessage2,
  IconRecycle,
  IconSquareRoundedX,
  IconStack2,
  IconUsers,
} from "@tabler/icons-react";
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

export function AdminDashboard() {
  const [status, setStatus] = useState<Status>("idle");
  const [statusMessage, setStatusMessage] = useState("idle");
  const [adminTokenInput, setAdminTokenInput] = useState("");
  const [adminToken, setAdminToken] = useState("");
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

  const applyToken = () => {
    const token = adminTokenInput.trim();
    if (!token) {
      setStatusMessage("admin token is required");
      return;
    }
    setAdminToken(token);
    setStatusMessage("admin token applied (memory only)");
  };

  const clearToken = () => {
    setAdminTokenInput("");
    setAdminToken("");
    setStatus("idle");
    setStatusMessage("admin token cleared");
    setOverview(null);
    setRooms([]);
    setAudit([]);
  };

  const tokenMissing = adminToken.trim() === "";
  const effectiveStatusMessage = tokenMissing ? "missing admin token" : statusMessage;
  const tone =
    status === "ok"
      ? "good"
      : status === "error"
        ? "bad"
        : status === "loading"
          ? "warn"
          : "muted";

  return (
    <section className="admin-shell">
      <section className="wb-card admin-auth-card">
        <div className="admin-auth-head">
          <div>
            <h2>Admin Token</h2>
            <p>Authorize dashboard metrics and room visibility endpoints. Token is memory-only and clears on refresh.</p>
          </div>
          <span className={`admin-status-chip ${tone}`}>{status}</span>
        </div>

        <div className="admin-auth-row">
          <input
            type="password"
            value={adminTokenInput}
            onChange={(e) => setAdminTokenInput(e.target.value)}
            placeholder="ADMIN_TOKEN"
            className="admin-token-input"
          />
          <button onClick={applyToken} className="admin-btn primary">
            Use Token
          </button>
          <button onClick={clearToken} className="admin-btn ghost">
            Clear
          </button>
        </div>

        <div className="admin-auth-status">
          API status: <strong className={`tone-${tone}`}>{effectiveStatusMessage}</strong>
        </div>
      </section>

      <div className="admin-stats-grid">
        <StatCard label="Agents" value={overview?.agents_total ?? 0} icon={<IconUsers size={16} />} />
        <StatCard
          label="Sessions"
          value={overview?.sessions_active ?? 0}
          icon={<IconBrandSpeedtest size={16} />}
        />
        <StatCard
          label="Messages"
          value={overview?.messages_total ?? 0}
          icon={<IconMessage2 size={16} />}
        />
        <StatCard
          label="Rooms Open"
          value={overview?.rooms_open ?? 0}
          icon={<IconStack2 size={16} />}
        />
        <StatCard
          label="Rooms Active"
          value={overview?.rooms_active ?? 0}
          icon={<IconActivityHeartbeat size={16} />}
        />
        <StatCard
          label="Rooms Closed"
          value={overview?.rooms_closed ?? 0}
          icon={<IconSquareRoundedX size={16} />}
        />
        <StatCard
          label="Rooms Purged"
          value={overview?.rooms_purged ?? 0}
          icon={<IconRecycle size={16} />}
        />
      </div>

      <section className="wb-card admin-table-card">
        <div className="admin-table-head">
          <h3>Rooms</h3>
          <p>{roomSummary.map(([state, count]) => `${state}: ${count}`).join(" | ") || "No rooms"}</p>
        </div>
        <div className="admin-table-scroll">
          <table className="admin-table">
            <thead>
              <tr>
                {["room_id", "state", "turn", "agent_a", "agent_b", "created_at"].map((h) => (
                  <th key={h}>{h}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {rooms.map((room) => (
                <tr key={room.id}>
                  <td>{room.id}</td>
                  <td>{room.state}</td>
                  <td>
                    {room.turn_index}/{room.max_turns}
                  </td>
                  <td>{room.agent_a_name || room.agent_a_id}</td>
                  <td>{room.agent_b_name || room.agent_b_id}</td>
                  <td>{new Date(room.created_at).toLocaleString()}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>

      <section className="wb-card admin-table-card">
        <div className="admin-table-head">
          <h3>Audit</h3>
          <p>Most recent timeline records.</p>
        </div>
        <div className="admin-table-scroll">
          <table className="admin-table">
            <thead>
              <tr>
                {["id", "event", "room_id", "message_count", "created_at"].map((h) => (
                  <th key={h}>{h}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {audit.map((ev) => (
                <tr key={ev.id}>
                  <td>{ev.id}</td>
                  <td>{ev.event}</td>
                  <td>{ev.room_id}</td>
                  <td>{ev.message_count}</td>
                  <td>{new Date(ev.created_at).toLocaleString()}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>
    </section>
  );
}

function StatCard({ label, value, icon }: { label: string; value: number; icon: ReactNode }) {
  return (
    <article className="wb-card admin-stat-card">
      <div className="admin-stat-head">
        <span>{label}</span>
        <span className="admin-stat-icon">{icon}</span>
      </div>
      <div className="admin-stat-value">{value}</div>
    </article>
  );
}

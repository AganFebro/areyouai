"use client";

import { useState } from "react";
import { useEffect } from "react";
import { config } from "@/lib/config";

type TranscriptMessage = {
  id: string;
  sender_id: string;
  turn: number;
  ciphertext: string;
  created_at: string;
};

export function HumanRoomTester() {
  const [roomID, setRoomID] = useState("");
  const [humanCode, setHumanCode] = useState("");
  const [viewerToken, setViewerToken] = useState("");
  const [status, setStatus] = useState("idle");
  const [messages, setMessages] = useState<TranscriptMessage[]>([]);
  const [autoRefresh, setAutoRefresh] = useState(false);

  const postViewer = async (op: "join" | "heartbeat" | "leave") => {
    if (!roomID.trim()) {
      setStatus("room_id is required");
      return;
    }
    if (op === "join" && !humanCode.trim()) {
      setStatus("human_code is required for join");
      return;
    }
    if ((op === "heartbeat" || op === "leave") && !viewerToken.trim()) {
      setStatus("viewer_token is required");
      return;
    }

    setStatus(`${op}...`);
    try {
      const res = await fetch(`${config.apiBaseUrl}/v1/rooms/${roomID}/viewers`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          op,
          human_code: humanCode,
          viewer_token: viewerToken,
        }),
      });
      const data = await res.json();
      if (!res.ok) {
        setStatus(`${op} failed: ${data.error ?? res.status}`);
        return;
      }
      if (op === "join" && data.viewer_token) {
        setViewerToken(String(data.viewer_token));
      }
      setStatus(`${op} ok`);
      if (op === "join") {
        setAutoRefresh(true);
        await loadTranscriptInternal();
      }
      if (op === "leave") {
        setAutoRefresh(false);
      }
    } catch {
      setStatus(`${op} failed: network error`);
    }
  };

  const loadTranscriptInternal = async () => {
    if (!roomID.trim() || !humanCode.trim()) {
      setStatus("room_id and human_code are required");
      return;
    }
    try {
      const res = await fetch(
        `${config.apiBaseUrl}/v1/rooms/${roomID}/transcript?human_code=${encodeURIComponent(humanCode)}`,
        { cache: "no-store" },
      );
      const data = await res.json();
      if (!res.ok) {
        setStatus(`transcript failed: ${data.error ?? res.status}`);
        if (res.status === 410) {
          setAutoRefresh(false);
        }
        return;
      }
      const raw = Array.isArray(data.messages) ? data.messages : [];
      const normalized = raw.map(normalizeMessage).filter(Boolean) as TranscriptMessage[];
      setMessages(normalized);
      setStatus(autoRefresh ? "live refresh active" : "transcript loaded");
    } catch {
      setStatus("transcript failed: network error");
    }
  };

  const loadTranscript = async () => {
    setStatus("loading transcript...");
    await loadTranscriptInternal();
  };

  useEffect(() => {
    if (!autoRefresh || !viewerToken.trim()) return;
    const id = setInterval(() => {
      void postViewer("heartbeat");
      void loadTranscriptInternal();
    }, 3000);
    return () => clearInterval(id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [autoRefresh, viewerToken, roomID, humanCode]);

  return (
    <section
      style={{
        marginTop: 24,
        border: "1px solid #334155",
        borderRadius: 12,
        padding: 16,
        background: "#0b1220",
      }}
    >
      <h2 style={{ marginTop: 0 }}>Human Room Tester</h2>
      <p style={{ marginTop: 0, color: "#94a3b8" }}>
        Join as viewer and load transcript using <code>room_id</code> + <code>human_code</code>.
      </p>

      <div style={{ display: "grid", gap: 8 }}>
        <input
          value={roomID}
          onChange={(e) => setRoomID(e.target.value)}
          placeholder="room_id"
          style={inputStyle}
        />
        <input
          value={humanCode}
          onChange={(e) => setHumanCode(e.target.value)}
          placeholder="human_code"
          style={inputStyle}
        />
        <input
          value={viewerToken}
          onChange={(e) => setViewerToken(e.target.value)}
          placeholder="viewer_token (after join)"
          style={inputStyle}
        />
      </div>

      <div style={{ marginTop: 12, display: "flex", gap: 8, flexWrap: "wrap" }}>
        <button onClick={() => postViewer("join")} style={btnStyle}>
          Join Viewer
        </button>
        <button onClick={() => postViewer("heartbeat")} style={btnStyle}>
          Heartbeat
        </button>
        <button onClick={() => postViewer("leave")} style={btnStyle}>
          Leave
        </button>
        <button onClick={loadTranscript} style={btnStyle}>
          Load Transcript
        </button>
      </div>

      <p style={{ marginTop: 12, marginBottom: 8 }}>
        Status: <strong>{status}</strong>
      </p>

      <div style={{ display: "grid", gap: 8 }}>
        {messages.map((m) => (
          <article
            key={m.id}
            style={{ border: "1px solid #1f2937", borderRadius: 8, padding: 10, background: "#020617" }}
          >
            <div style={{ color: "#94a3b8", fontSize: 13 }}>
              turn {m.turn} | sender {m.sender_id}
            </div>
            <div style={{ marginTop: 6, whiteSpace: "pre-wrap" }}>{m.ciphertext}</div>
          </article>
        ))}
      </div>
    </section>
  );
}

function normalizeMessage(raw: any): TranscriptMessage | null {
  if (!raw || typeof raw !== "object") return null;
  return {
    id: String(raw.id ?? raw.ID ?? ""),
    sender_id: String(raw.sender_id ?? raw.SenderID ?? ""),
    turn: Number(raw.turn ?? raw.Turn ?? 0),
    ciphertext: String(raw.ciphertext ?? raw.Ciphertext ?? ""),
    created_at: String(raw.created_at ?? raw.CreatedAt ?? ""),
  };
}

const inputStyle: React.CSSProperties = {
  width: "100%",
  borderRadius: 8,
  border: "1px solid #334155",
  background: "#020617",
  color: "#e5e7eb",
  padding: "10px 12px",
};

const btnStyle: React.CSSProperties = {
  borderRadius: 8,
  border: "1px solid #334155",
  background: "#0f172a",
  color: "#e5e7eb",
  padding: "8px 12px",
  cursor: "pointer",
};

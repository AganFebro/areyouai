"use client";

import { useState } from "react";
import { useEffect } from "react";
import {
    IconHeartbeat,
    IconLogin2,
    IconLogout2,
    IconReload,
    IconRss,
} from "@tabler/icons-react";
import { config } from "@/lib/config";

type TranscriptMessage = {
    id: string;
    sender_id: string;
    sender_name?: string;
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
            const res = await fetch(
                `${config.apiBaseUrl}/v1/rooms/${roomID}/viewers`,
                {
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify({
                        op,
                        human_code: humanCode,
                        viewer_token: viewerToken,
                    }),
                },
            );
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
            const normalized = raw
                .map(normalizeMessage)
                .filter(Boolean) as TranscriptMessage[];
            setMessages(normalized);
            setStatus(
                autoRefresh ? "live refresh active" : "transcript loaded",
            );
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

    const statusClass =
        status.includes("failed") || status.includes("required")
            ? "status-bad"
            : status.includes("ok") || status.includes("live")
              ? "status-good"
              : "status-muted";

    return (
        <section className="room-shell">
            <aside className="viewer-controls">
                <div className="viewer-controls-head">
                    <h2>Join Agent Room</h2>
                </div>

                <div className="viewer-field">
                    <label>Room ID</label>
                    <input
                        value={roomID}
                        onChange={(e) => setRoomID(e.target.value)}
                        placeholder="room_xxx"
                    />
                </div>
                <div className="viewer-field">
                    <label>Human Code</label>
                    <input
                        value={humanCode}
                        onChange={(e) => setHumanCode(e.target.value)}
                        placeholder="hc_xxx"
                        type="password"
                    />
                </div>
                <div className="viewer-field">
                    <label>Viewer Token</label>
                    <input
                        value={viewerToken}
                        onChange={(e) => setViewerToken(e.target.value)}
                        placeholder="hv_xxx"
                    />
                </div>

                <div className="viewer-actions">
                    <button
                        onClick={() => postViewer("join")}
                        className="btn-primary"
                    >
                        <IconLogin2 size={14} />
                        JOIN_ROOM
                    </button>
                    <button
                        onClick={() => postViewer("heartbeat")}
                        className="btn-secondary"
                    >
                        <IconHeartbeat size={14} />
                        HEARTBEAT
                    </button>
                    <button onClick={loadTranscript} className="btn-secondary">
                        <IconReload size={14} />
                        LOAD_TRANSCRIPT
                    </button>
                    <button
                        onClick={() => postViewer("leave")}
                        className="btn-danger"
                    >
                        <IconLogout2 size={14} />
                        LEAVE
                    </button>
                </div>
            </aside>

            <section className="viewer-transcript">
                <header className="transcript-topbar">
                    <div className="transcript-live">
                        <span className="live-dot" />
                        <IconRss size={14} />
                        <span>LIVE_CHAT</span>
                    </div>
                    <div className="transcript-meta">
                        <span className="chip">room: {roomID || "-"}</span>
                        <span className="chip">
                            messages: {messages.length}
                        </span>
                    </div>
                </header>

                <div className="transcript-status">
                    status: <strong className={statusClass}>{status}</strong>
                </div>

                <div className="transcript-list">
                    {messages.length === 0 && (
                        <div className="transcript-empty">
                            No chat loaded yet. Join viewer and wait for
                            messages.
                        </div>
                    )}

                    {messages.map((m) => (
                        <article key={m.id} className="message-row">
                            <div className="message-head">
                                turn {m.turn} | sender{" "}
                                {m.sender_name || m.sender_id}
                            </div>
                            <div className="message-body">{m.ciphertext}</div>
                        </article>
                    ))}
                </div>
            </section>
        </section>
    );
}

function normalizeMessage(raw: unknown): TranscriptMessage | null {
    if (!raw || typeof raw !== "object") return null;
    const msg = raw as Record<string, unknown>;
    return {
        id: String(msg.id ?? msg.ID ?? ""),
        sender_id: String(msg.sender_id ?? msg.SenderID ?? ""),
        sender_name: String(msg.sender_name ?? msg.SenderName ?? ""),
        turn: Number(msg.turn ?? msg.Turn ?? 0),
        ciphertext: String(msg.ciphertext ?? msg.Ciphertext ?? ""),
        created_at: String(msg.created_at ?? msg.CreatedAt ?? ""),
    };
}

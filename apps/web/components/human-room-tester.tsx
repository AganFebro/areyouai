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
import { MarkdownMessage } from "@/components/markdown-message";

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
    const [roomTopic, setRoomTopic] = useState("");
    const [agentAID, setAgentAID] = useState("");
    const [agentBID, setAgentBID] = useState("");
    const [roomTurnIndex, setRoomTurnIndex] = useState(0);
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
                await loadTranscriptInternal(true);
            }
            if (op === "leave") {
                setAutoRefresh(false);
            }
        } catch {
            setStatus(`${op} failed: network error`);
        }
    };

    const loadTranscriptInternal = async (liveRefresh = autoRefresh) => {
        if (!roomID.trim() || !humanCode.trim()) {
            setStatus("room_id and human_code are required");
            return;
        }
        try {
            const res = await fetch(
                `${config.apiBaseUrl}/v1/rooms/${roomID}/transcript`,
                {
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify({ human_code: humanCode }),
                    cache: "no-store",
                },
            );
            const data = await res.json();
            if (!res.ok) {
                setStatus(`transcript failed: ${data.error ?? res.status}`);
                if (res.status === 410) {
                    setAutoRefresh(false);
                }
                return;
            }
            if (typeof data.room_topic === "string") {
                setRoomTopic(data.room_topic);
            }
            if (typeof data.agent_a_id === "string") {
                setAgentAID(data.agent_a_id);
            }
            if (typeof data.agent_b_id === "string") {
                setAgentBID(data.agent_b_id);
            }
            if (typeof data.turn_index === "number") {
                setRoomTurnIndex(data.turn_index);
            }
            const raw = Array.isArray(data.messages) ? data.messages : [];
            const normalized = raw
                .map(normalizeMessage)
                .filter(Boolean) as TranscriptMessage[];
            setMessages(normalized);
            setStatus(liveRefresh ? "live refresh active" : "transcript loaded");
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
            void loadTranscriptInternal(true);
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
                        <span className="chip topic-chip">
                            topic: {roomTopic || "-"}
                        </span>
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
                        <article
                            key={m.id}
                            className={`message-row ${getSenderRole(
                                m.sender_id,
                                agentAID,
                                agentBID,
                            )}`}
                        >
                            <div className="message-head message-head-row">
                                <span>
                                    turn {m.turn} | {getSenderLabel(m, agentAID, agentBID)}
                                </span>
                                <span className={`message-status ${getMessageStatus(m.turn, roomTurnIndex)}`}>
                                    {getMessageStatus(m.turn, roomTurnIndex).toUpperCase()}
                                </span>
                            </div>
                            <MarkdownMessage content={m.ciphertext} />
                        </article>
                    ))}
                </div>
            </section>
        </section>
    );
}

function getMessageStatus(turn: number, roomTurnIndex: number): "sent" | "read" {
    if (turn < roomTurnIndex - 1) return "read";
    return "sent";
}

function getSenderRole(
    senderID: string,
    agentAID: string,
    agentBID: string,
): "agent-a" | "agent-b" | "unknown" {
    if (senderID && agentAID && senderID === agentAID) return "agent-a";
    if (senderID && agentBID && senderID === agentBID) return "agent-b";
    return "unknown";
}

function getSenderLabel(
    msg: TranscriptMessage,
    agentAID: string,
    agentBID: string,
): string {
    const role = getSenderRole(msg.sender_id, agentAID, agentBID);
    if (role === "agent-a") return `agent A · ${msg.sender_name || msg.sender_id}`;
    if (role === "agent-b") return `agent B · ${msg.sender_name || msg.sender_id}`;
    return msg.sender_name || msg.sender_id;
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

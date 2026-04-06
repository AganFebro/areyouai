"use client";

import { useEffect, useState } from "react";
import {
    IconHeartbeat,
    IconLogin2,
    IconLogout2,
    IconReload,
    IconRss,
} from "@tabler/icons-react";
import { MarkdownMessage } from "@/components/markdown-message";
import { config } from "@/lib/config";

type TranscriptMessage = {
    id: string;
    sender_id: string;
    sender_name?: string;
    turn: number;
    ciphertext: string;
    created_at: string;
    read_by_opponent?: boolean;
};

type ViewerTypingEvent = {
    type: string;
    room_id: string;
    actor_id: string;
    state: "start" | "stop";
    ttl_ms?: number;
    created_at: string;
    expires_at: string;
};

type TypingPresence = {
    actorID: string;
    expiresAt: number;
};

const viewerEventsReconnectDelayMS = 1000;

export function HumanRoomTester() {
    const [roomID, setRoomID] = useState("");
    const [humanCode, setHumanCode] = useState("");
    const [viewerToken, setViewerToken] = useState("");
    const [status, setStatus] = useState("idle");
    const [viewerStreamStatus, setViewerStreamStatus] = useState("idle");
    const [roomTopic, setRoomTopic] = useState("");
    const [messages, setMessages] = useState<TranscriptMessage[]>([]);
    const [typingByActor, setTypingByActor] = useState<
        Record<string, TypingPresence>
    >({});
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
            const data = await parseJSONResponse(res);
            if (!res.ok) {
                setStatus(`${op} failed: ${data?.error ?? res.status}`);
                if (res.status === 404 || res.status === 410) {
                    setAutoRefresh(false);
                    setTypingByActor({});
                    setViewerStreamStatus("idle");
                }
                return;
            }
            if (op === "join" && typeof data?.viewer_token === "string") {
                setViewerToken(data.viewer_token);
            }
            setStatus(`${op} ok`);
            if (op === "join") {
                setTypingByActor({});
                setAutoRefresh(true);
                setViewerStreamStatus("viewer events connecting");
                void loadTranscriptInternal(true);
            }
            if (op === "leave") {
                setAutoRefresh(false);
                setTypingByActor({});
                setViewerStreamStatus("idle");
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
            const res = await fetch(`${config.apiBaseUrl}/v1/rooms/${roomID}/transcript`, {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ human_code: humanCode }),
                cache: "no-store",
            });
            const data = await parseJSONResponse(res);
            if (!res.ok) {
                setStatus(`transcript failed: ${data?.error ?? res.status}`);
                if (res.status === 410) {
                    setAutoRefresh(false);
                    setTypingByActor({});
                    setViewerStreamStatus("idle");
                }
                return;
            }
            if (typeof data?.room_topic === "string") {
                setRoomTopic(data.room_topic);
            }
            const raw = Array.isArray(data?.messages) ? data.messages : [];
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
        if (!autoRefresh || !viewerToken.trim()) {
            return;
        }
        const id = setInterval(() => {
            void postViewer("heartbeat");
            void loadTranscriptInternal(true);
        }, 3000);
        return () => clearInterval(id);
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [autoRefresh, viewerToken, roomID, humanCode]);

    useEffect(() => {
        if (!autoRefresh || !roomID.trim() || !viewerToken.trim()) {
            setTypingByActor({});
            setViewerStreamStatus("idle");
            return;
        }

        const controller = new AbortController();
        setViewerStreamStatus("viewer events connecting");

        const connect = async () => {
            while (!controller.signal.aborted) {
                try {
                    const res = await fetch(
                        `${config.apiBaseUrl}/v1/rooms/${roomID}/viewer-events`,
                        {
                            method: "GET",
                            headers: {
                                Authorization: `Bearer ${viewerToken}`,
                            },
                            cache: "no-store",
                            signal: controller.signal,
                        },
                    );

                    if (!res.ok) {
                        const error = await readAPIError(res);
                        setTypingByActor({});
                        setViewerStreamStatus(`viewer events failed: ${error}`);
                        if (res.status === 404 || res.status === 410) {
                            setAutoRefresh(false);
                            return;
                        }
                    } else if (!res.body) {
                        setTypingByActor({});
                        setViewerStreamStatus("viewer events failed: no stream body");
                    } else {
                        setViewerStreamStatus("viewer events connected");
                        await readViewerEventsStream(
                            res.body,
                            (event) => {
                                if (event.room_id !== roomID) {
                                    return;
                                }
                                setTypingByActor((current) =>
                                    applyTypingEvent(current, event),
                                );
                            },
                            controller.signal,
                        );
                        if (controller.signal.aborted) {
                            return;
                        }
                        setTypingByActor({});
                        setViewerStreamStatus("viewer events reconnecting");
                    }
                } catch {
                    if (controller.signal.aborted) {
                        return;
                    }
                    setTypingByActor({});
                    setViewerStreamStatus("viewer events reconnecting");
                }

                await sleep(viewerEventsReconnectDelayMS);
            }
        };

        void connect();
        return () => controller.abort();
    }, [autoRefresh, roomID, viewerToken]);

    useEffect(() => {
        if (Object.keys(typingByActor).length === 0) {
            return;
        }
        const id = window.setInterval(() => {
            setTypingByActor((current) => pruneExpiredTyping(current, Date.now()));
        }, 500);
        return () => window.clearInterval(id);
    }, [typingByActor]);

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
                        <span
                            className={`chip viewer-stream-chip ${getViewerStreamTone(viewerStreamStatus)}`}
                        >
                            stream: {viewerStreamStatus}
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

                    {messages.map((message) => (
                        <article
                            key={message.id}
                            className={`message-row ${getSenderRole(message.turn)}`}
                        >
                            <div className="message-head message-head-row">
                                <span>
                                    turn {message.turn} |{" "}
                                    {getSenderLabel(message)}
                                </span>
                                <span
                                    className={`message-status ${getMessageStatus(
                                        message,
                                    )}`}
                                >
                                    {getMessageStatus(
                                        message,
                                    ).toUpperCase()}
                                </span>
                            </div>
                            <MarkdownMessage content={message.ciphertext} />
                        </article>
                    ))}
                </div>
            </section>
        </section>
    );
}

function getMessageStatus(message: TranscriptMessage): "sent" | "read" {
    if (message.read_by_opponent) {
        return "read";
    }
    return "sent";
}

function getSenderRole(turn: number): "agent-a" | "agent-b" {
    return turn % 2 === 0 ? "agent-b" : "agent-a";
}

function getSenderLabel(
    message: TranscriptMessage,
): string {
    const role = getSenderRole(message.turn);
    if (role === "agent-a") {
        return `agent A · ${message.sender_name || message.sender_id}`;
    }
    if (role === "agent-b") {
        return `agent B · ${message.sender_name || message.sender_id}`;
    }
    return message.sender_name || message.sender_id;
}

function normalizeMessage(raw: unknown): TranscriptMessage | null {
    if (!raw || typeof raw !== "object") {
        return null;
    }
    const message = raw as Record<string, unknown>;
    return {
        id: String(message.id ?? message.ID ?? ""),
        sender_id: String(message.sender_id ?? message.SenderID ?? ""),
        sender_name: String(message.sender_name ?? message.SenderName ?? ""),
        turn: Number(message.turn ?? message.Turn ?? 0),
        ciphertext: String(message.ciphertext ?? message.Ciphertext ?? ""),
        created_at: String(message.created_at ?? message.CreatedAt ?? ""),
        read_by_opponent:
            typeof message.read_by_opponent === "boolean"
                ? message.read_by_opponent
                : typeof message.read_by_opponent === "string"
                  ? message.read_by_opponent === "true"
                  : undefined,
    };
}

async function parseJSONResponse(
    res: Response,
): Promise<Record<string, unknown> | null> {
    try {
        return (await res.json()) as Record<string, unknown>;
    } catch {
        return null;
    }
}

async function readAPIError(res: Response): Promise<string> {
    const data = await parseJSONResponse(res);
    if (typeof data?.error === "string" && data.error.trim()) {
        return data.error;
    }
    return String(res.status);
}

async function readViewerEventsStream(
    stream: ReadableStream<Uint8Array>,
    onEvent: (event: ViewerTypingEvent) => void,
    signal: AbortSignal,
): Promise<void> {
    const reader = stream.getReader();
    const decoder = new TextDecoder();
    let buffer = "";

    try {
        while (!signal.aborted) {
            const { done, value } = await reader.read();
            if (done) {
                break;
            }
            buffer += decoder.decode(value, { stream: true }).replace(/\r/g, "");

            const frames = buffer.split("\n\n");
            buffer = frames.pop() ?? "";
            for (const frame of frames) {
                const parsed = parseSSEFrame(frame);
                if (!parsed || parsed.eventType !== "agent.typing") {
                    continue;
                }
                const event = parseViewerTypingEvent(parsed.data);
                if (event) {
                    onEvent(event);
                }
            }
        }

        buffer += decoder.decode();
        const trailing = parseSSEFrame(buffer.replace(/\r/g, ""));
        if (!signal.aborted && trailing?.eventType === "agent.typing") {
            const event = parseViewerTypingEvent(trailing.data);
            if (event) {
                onEvent(event);
            }
        }
    } finally {
        reader.releaseLock();
    }
}

function parseSSEFrame(
    frame: string,
): { eventType: string; data: string } | null {
    const lines = frame.split("\n");
    let eventType = "message";
    const dataLines: string[] = [];

    for (const line of lines) {
        if (!line || line.startsWith(":")) {
            continue;
        }
        if (line.startsWith("event:")) {
            eventType = line.slice("event:".length).trim();
            continue;
        }
        if (line.startsWith("data:")) {
            dataLines.push(line.slice("data:".length).trimStart());
        }
    }

    if (dataLines.length === 0) {
        return null;
    }

    return {
        eventType,
        data: dataLines.join("\n"),
    };
}

function parseViewerTypingEvent(data: string): ViewerTypingEvent | null {
    try {
        const raw = JSON.parse(data) as Record<string, unknown>;
        if (
            typeof raw.type !== "string" ||
            typeof raw.room_id !== "string" ||
            typeof raw.actor_id !== "string" ||
            (raw.state !== "start" && raw.state !== "stop") ||
            typeof raw.expires_at !== "string"
        ) {
            return null;
        }
        return {
            type: raw.type,
            room_id: raw.room_id,
            actor_id: raw.actor_id,
            state: raw.state,
            ttl_ms:
                typeof raw.ttl_ms === "number" ? raw.ttl_ms : undefined,
            created_at:
                typeof raw.created_at === "string" ? raw.created_at : "",
            expires_at: raw.expires_at,
        };
    } catch {
        return null;
    }
}

function applyTypingEvent(
    current: Record<string, TypingPresence>,
    event: ViewerTypingEvent,
): Record<string, TypingPresence> {
    if (event.state === "stop") {
        return removeTypingPresence(current, event.actor_id);
    }

    const expiresAt = Date.parse(event.expires_at);
    if (Number.isNaN(expiresAt)) {
        return current;
    }

    const existing = current[event.actor_id];
    if (existing && existing.expiresAt === expiresAt) {
        return current;
    }

    return {
        ...current,
        [event.actor_id]: {
            actorID: event.actor_id,
            expiresAt,
        },
    };
}

function removeTypingPresence(
    current: Record<string, TypingPresence>,
    actorID: string,
): Record<string, TypingPresence> {
    if (!current[actorID]) {
        return current;
    }

    const next = { ...current };
    delete next[actorID];
    return next;
}

function pruneExpiredTyping(
    current: Record<string, TypingPresence>,
    now: number,
): Record<string, TypingPresence> {
    let changed = false;
    const next: Record<string, TypingPresence> = {};

    for (const [actorID, presence] of Object.entries(current)) {
        if (presence.expiresAt <= now) {
            changed = true;
            continue;
        }
        next[actorID] = presence;
    }

    return changed ? next : current;
}

function getViewerStreamTone(status: string): string {
    if (status.includes("connected")) {
        return "viewer-stream-good";
    }
    if (status.includes("failed")) {
        return "viewer-stream-bad";
    }
    return "viewer-stream-muted";
}

function sleep(ms: number): Promise<void> {
    return new Promise((resolve) => {
        window.setTimeout(resolve, ms);
    });
}

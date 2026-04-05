"use client";

import { useState } from "react";
import { IconCopy, IconCopyCheck } from "@tabler/icons-react";

type InstallSnippetProps = {
    title: string;
    copyLabel: string;
    command: string;
};

export function InstallSnippet({
    title,
    copyLabel,
    command,
}: InstallSnippetProps) {
    const [copied, setCopied] = useState(false);

    const onCopy = async () => {
        try {
            await navigator.clipboard.writeText(command);
            setCopied(true);
            window.setTimeout(() => setCopied(false), 1200);
        } catch {
            setCopied(false);
        }
    };

    return (
        <section className="install-card">
            <h2 className="install-title">{title}</h2>
            <p className="install-copy-label">{copyLabel}</p>

            <div className="install-snippet">
                <div className="install-snippet-top">
                    <div className="install-dots" aria-hidden>
                        <span />
                        <span />
                        <span />
                    </div>
                    <span className="install-platform">OpenClaw</span>
                </div>

                <div className="install-command-row">
                    <span className="install-prompt">$</span>
                    <code className="install-command">{command}</code>
                    <button
                        onClick={onCopy}
                        className="install-copy-btn"
                        aria-label="Copy install command"
                    >
                        {copied ? (
                            <IconCopyCheck size={14} />
                        ) : (
                            <IconCopy size={14} />
                        )}
                        <span>{copied ? "Copied" : "Copy"}</span>
                    </button>
                </div>
            </div>
        </section>
    );
}

import { Fragment, type ReactNode } from "react";

type MarkdownMessageProps = {
  content: string;
};

type InlineToken = {
  index: number;
  length: number;
  kind: "code" | "link" | "strong" | "strike" | "emphasis";
  text: string;
  href?: string;
};

export function MarkdownMessage({ content }: MarkdownMessageProps) {
  return <div className="markdown-message">{renderBlocks(content)}</div>;
}

function renderBlocks(content: string): ReactNode[] {
  const lines = content.replace(/\r\n/g, "\n").replace(/\r/g, "\n").split("\n");
  const out: ReactNode[] = [];

  let i = 0;
  while (i < lines.length) {
    const line = lines[i] ?? "";
    if (!line.trim()) {
      i += 1;
      continue;
    }

    if (line.startsWith("```")) {
      const block: string[] = [];
      i += 1;
      while (i < lines.length && !(lines[i] ?? "").startsWith("```")) {
        block.push(lines[i] ?? "");
        i += 1;
      }
      if (i < lines.length) i += 1;
      out.push(
        <pre key={`code-${out.length}`}>
          <code>{block.join("\n")}</code>
        </pre>,
      );
      continue;
    }

    if (/^>\s?/.test(line)) {
      const quote: string[] = [];
      while (i < lines.length && /^>\s?/.test(lines[i] ?? "")) {
        quote.push((lines[i] ?? "").replace(/^>\s?/, ""));
        i += 1;
      }
      out.push(
        <blockquote key={`quote-${out.length}`}>
          <p>{renderInline(quote.join(" "))}</p>
        </blockquote>,
      );
      continue;
    }

    if (/^(- |\* )/.test(line)) {
      const items: string[] = [];
      while (i < lines.length && /^(- |\* )/.test(lines[i] ?? "")) {
        items.push((lines[i] ?? "").replace(/^(- |\* )/, ""));
        i += 1;
      }
      out.push(
        <ul key={`ul-${out.length}`}>
          {items.map((item, idx) => (
            <li key={idx}>{renderInline(item)}</li>
          ))}
        </ul>,
      );
      continue;
    }

    if (/^\d+\.\s+/.test(line)) {
      const items: string[] = [];
      while (i < lines.length && /^\d+\.\s+/.test(lines[i] ?? "")) {
        items.push((lines[i] ?? "").replace(/^\d+\.\s+/, ""));
        i += 1;
      }
      out.push(
        <ol key={`ol-${out.length}`}>
          {items.map((item, idx) => (
            <li key={idx}>{renderInline(item)}</li>
          ))}
        </ol>,
      );
      continue;
    }

    const paragraph: string[] = [];
    while (
      i < lines.length &&
      (lines[i] ?? "").trim() &&
      !(lines[i] ?? "").startsWith("```") &&
      !/^>\s?/.test(lines[i] ?? "") &&
      !/^(- |\* )/.test(lines[i] ?? "") &&
      !/^\d+\.\s+/.test(lines[i] ?? "")
    ) {
      paragraph.push(lines[i] ?? "");
      i += 1;
    }
    out.push(<p key={`p-${out.length}`}>{renderInline(paragraph.join(" "))}</p>);
  }

  return out;
}

function renderInline(text: string): ReactNode[] {
  const out: ReactNode[] = [];
  let remaining = text;

  while (remaining.length > 0) {
    const token = findNextToken(remaining);
    if (!token) {
      out.push(remaining);
      break;
    }

    if (token.index > 0) {
      out.push(remaining.slice(0, token.index));
    }

    const inner = token.text;
    switch (token.kind) {
      case "code":
        out.push(
          <code key={out.length}>
            {inner}
          </code>,
        );
        break;
      case "link":
        {
          const safeHref = sanitizeLinkHref(token.href);
          out.push(
            safeHref ? (
              <a key={out.length} href={safeHref} target="_blank" rel="noreferrer noopener">
                {renderInline(inner)}
              </a>
            ) : (
              <Fragment key={out.length}>{renderInline(inner)}</Fragment>
            ),
          );
        }
        break;
      case "strong":
        out.push(<strong key={out.length}>{renderInline(inner)}</strong>);
        break;
      case "strike":
        out.push(<s key={out.length}>{renderInline(inner)}</s>);
        break;
      case "emphasis":
        out.push(<em key={out.length}>{renderInline(inner)}</em>);
        break;
    }

    remaining = remaining.slice(token.index + token.length);
  }

  return out;
}

function findNextToken(text: string): InlineToken | null {
  const candidates: InlineToken[] = [];

  const code = /`([^`]+)`/.exec(text);
  if (code?.index != null) {
    candidates.push({ index: code.index, length: code[0].length, kind: "code", text: code[1] ?? "" });
  }

  const link = /\[([^\]]+)\]\(([^)\s]+)\)/.exec(text);
  if (link?.index != null) {
    candidates.push({ index: link.index, length: link[0].length, kind: "link", text: link[1] ?? "", href: link[2] ?? "" });
  }

  const strong = /\*\*([^*]+)\*\*/.exec(text);
  if (strong?.index != null) {
    candidates.push({ index: strong.index, length: strong[0].length, kind: "strong", text: strong[1] ?? "" });
  }

  const strike = /~~([^~]+)~~/.exec(text);
  if (strike?.index != null) {
    candidates.push({ index: strike.index, length: strike[0].length, kind: "strike", text: strike[1] ?? "" });
  }

  const em = /(?:\*([^*\n]+)\*|_([^_\n]+)_)/.exec(text);
  if (em?.index != null) {
    candidates.push({ index: em.index, length: em[0].length, kind: "emphasis", text: em[1] ?? em[2] ?? "" });
  }

  if (candidates.length === 0) {
    return null;
  }

  candidates.sort((a, b) => a.index - b.index || tokenPriority(a.kind) - tokenPriority(b.kind));
  return candidates[0] ?? null;
}

function tokenPriority(kind: InlineToken["kind"]): number {
  switch (kind) {
    case "code":
      return 0;
    case "link":
      return 1;
    case "strong":
      return 2;
    case "strike":
      return 3;
    case "emphasis":
      return 4;
  }
}

const safeLinkProtocols = new Set(["http:", "https:", "mailto:", "tel:"]);

function sanitizeLinkHref(href?: string): string | undefined {
  const trimmed = href?.trim();
  if (!trimmed) {
    return undefined;
  }

  if (!/^[a-zA-Z][a-zA-Z\d+\-.]*:/.test(trimmed)) {
    return undefined;
  }

  try {
    const parsed = new URL(trimmed);
    if (!safeLinkProtocols.has(parsed.protocol)) {
      return undefined;
    }
    return trimmed;
  } catch {
    return undefined;
  }
}

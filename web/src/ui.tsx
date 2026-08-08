// Small shared building blocks and formatters. Deliberately hand-rolled: the
// panel is a handful of tables, and a component library would outweigh it.

import { useCallback, useEffect, useRef, useState } from "react";

export function bytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v < 10 ? v.toFixed(1) : Math.round(v)} ${units[i]}`;
}

/** Compact relative time: "just now", "4m", "3h", "2d". */
export function ago(iso: string | undefined): string {
  if (!iso) return "never";
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return "—";
  const secs = Math.max(0, Math.round((Date.now() - then) / 1000));
  if (secs < 10) return "just now";
  if (secs < 60) return `${secs}s`;
  if (secs < 3600) return `${Math.floor(secs / 60)}m`;
  if (secs < 86400) return `${Math.floor(secs / 3600)}h`;
  return `${Math.floor(secs / 86400)}d`;
}

export function Badge({
  kind,
  children,
}: {
  kind?: "http" | "tcp" | "ok" | "warn" | "muted";
  children: React.ReactNode;
}) {
  return <span className={`badge badge-${kind ?? "muted"}`}>{children}</span>;
}

/** Copy-to-clipboard button that confirms inline for a moment. */
export function Copy({ value, label }: { value: string; label?: string }) {
  const [done, setDone] = useState(false);
  const timer = useRef<number | undefined>(undefined);

  useEffect(() => () => window.clearTimeout(timer.current), []);

  const copy = useCallback(async () => {
    try {
      await navigator.clipboard.writeText(value);
    } catch {
      // Clipboard access needs a secure context; over plain http the user
      // can still select the text, so a silent fallback is fine.
      return;
    }
    setDone(true);
    timer.current = window.setTimeout(() => setDone(false), 1200);
  }, [value]);

  return (
    <button type="button" className="btn btn-ghost" onClick={copy}>
      {done ? "copied" : (label ?? "copy")}
    </button>
  );
}

/** A secret that is hidden until asked for. */
export function Secret({ value }: { value: string }) {
  const [shown, setShown] = useState(false);
  return (
    <span className="secret">
      <code>{shown ? value : "•".repeat(Math.min(value.length, 24))}</code>
      <button
        type="button"
        className="btn btn-ghost"
        onClick={() => setShown((s) => !s)}
      >
        {shown ? "hide" : "show"}
      </button>
      <Copy value={value} />
    </span>
  );
}

/** Destructive action that asks once, inline, instead of via window.confirm. */
export function ConfirmButton({
  onConfirm,
  children,
  confirmLabel = "sure?",
  danger = true,
}: {
  onConfirm: () => void;
  children: React.ReactNode;
  confirmLabel?: string;
  danger?: boolean;
}) {
  const [armed, setArmed] = useState(false);
  const timer = useRef<number | undefined>(undefined);

  useEffect(() => () => window.clearTimeout(timer.current), []);

  if (armed) {
    return (
      <button
        type="button"
        className="btn btn-danger"
        onClick={() => {
          window.clearTimeout(timer.current);
          setArmed(false);
          onConfirm();
        }}
      >
        {confirmLabel}
      </button>
    );
  }
  return (
    <button
      type="button"
      className={danger ? "btn btn-ghost danger" : "btn btn-ghost"}
      onClick={() => {
        setArmed(true);
        // Disarm on its own so a stray click much later cannot destroy
        // anything.
        timer.current = window.setTimeout(() => setArmed(false), 4000);
      }}
    >
      {children}
    </button>
  );
}

export function Empty({ children }: { children: React.ReactNode }) {
  return <div className="empty">{children}</div>;
}

/** Parses "a, b,c" into a clean list. */
export function parseList(s: string): string[] {
  return s
    .split(/[\s,]+/)
    .map((v) => v.trim())
    .filter(Boolean);
}

/** Parses a comma-separated port list, rejecting anything out of range. */
export function parsePorts(s: string): { ports: number[]; error?: string } {
  const ports: number[] = [];
  for (const part of parseList(s)) {
    const n = Number(part);
    if (!Number.isInteger(n) || n < 1 || n > 65535) {
      return { ports: [], error: `"${part}" is not a valid port` };
    }
    ports.push(n);
  }
  return { ports };
}

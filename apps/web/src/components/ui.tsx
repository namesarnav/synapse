import { useEffect, type ReactNode } from "react";
import type { ExecState, NodeState } from "@protocol";
import { STATE_GLYPH } from "../lib/execution";

type Status = NodeState | ExecState | "draft" | "published";

const GLYPH: Record<string, string> = {
  ...STATE_GLYPH,
  created: "○",
  cancelling: "⊘",
  draft: "✎",
  published: "●",
};

/** Colour plus glyph plus text, so state never relies on colour alone. */
export function StatusBadge({ status }: { status: Status | string }) {
  return (
    <span className={`badge st-${status}`} data-status={status}>
      <span aria-hidden="true" className="glyph">
        {GLYPH[status] ?? "·"}
      </span>
      {status}
    </span>
  );
}

export function ErrorBanner({ error }: { error: string | null | undefined }) {
  if (!error) return null;
  return (
    <div className="banner error" role="alert">
      {error}
    </div>
  );
}

export function Empty({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <div className="empty">
      <p className="empty-title">{title}</p>
      {children}
    </div>
  );
}

export function Modal({ title, onClose, children }: { title: string; onClose: () => void; children: ReactNode }) {
  useEffect(() => {
    const h = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    window.addEventListener("keydown", h);
    return () => window.removeEventListener("keydown", h);
  }, [onClose]);
  return (
    <div className="modal-backdrop" onMouseDown={onClose}>
      <div className="modal" role="dialog" aria-modal="true" aria-label={title} onMouseDown={(e) => e.stopPropagation()}>
        <header>
          <h3>{title}</h3>
          <button className="ghost" onClick={onClose} aria-label="Close">
            ✕
          </button>
        </header>
        {children}
      </div>
    </div>
  );
}

export function ago(iso?: string, now = Date.now()): string {
  if (!iso) return "–";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "–";
  const s = Math.round((now - t) / 1000);
  if (s < 5) return "just now";
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}

export const shortId = (id: string) => id.slice(0, 8);

export function Json({ value }: { value: unknown }) {
  if (value === undefined || value === null) return <span className="muted">none</span>;
  return <pre className="json">{JSON.stringify(value, null, 2)}</pre>;
}

export const errMsg = (e: unknown): string => (e instanceof Error ? e.message : String(e));

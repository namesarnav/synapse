import { useEffect, useState } from "react";
import type { GraphNode, RetryPolicy } from "@protocol";
import { DEFAULT_RETRY, specFor, type Field } from "../lib/schema";

interface Props {
  node: GraphNode;
  readOnly?: boolean;
  onConfig: (key: string, value: unknown) => void;
  onPatch: (patch: Partial<GraphNode>) => void;
  onDelete: () => void;
}

export function ConfigPanel({ node, readOnly, onConfig, onPatch, onDelete }: Props) {
  const spec = specFor(node.type);
  const cfg = node.config ?? {};
  const retry = node.retry;
  return (
    <aside className="panel config" aria-label="Node configuration">
      <header>
        <h3>{spec?.label ?? node.type}</h3>
        {!readOnly && (
          <button className="ghost danger" onClick={onDelete}>
            Delete
          </button>
        )}
      </header>
      <fieldset disabled={readOnly}>
        <label className="field">
          <span>Name</span>
          <input value={node.name ?? ""} onChange={(e) => onPatch({ name: e.target.value })} />
        </label>
        <div className="field">
          <span>ID</span>
          <code>{node.id}</code>
        </div>
        {spec?.fields.map((f) => (
          <FieldEditor key={`${node.id}:${f.key}`} field={f} value={cfg[f.key]} onChange={(v) => onConfig(f.key, v)} />
        ))}

        <h4>Execution</h4>
        <label className="field">
          <span>Timeout (ms)</span>
          <input
            type="number"
            min={0}
            value={node.timeout_ms ?? ""}
            placeholder="default"
            onChange={(e) => onPatch({ timeout_ms: e.target.value === "" ? undefined : Number(e.target.value) })}
          />
        </label>
        <label className="field">
          <span>On error</span>
          <select value={node.on_error ?? "fail"} onChange={(e) => onPatch({ on_error: e.target.value as "fail" | "continue" })}>
            <option value="fail">fail the execution</option>
            <option value="continue">continue past this node</option>
          </select>
        </label>
        <label className="field inline">
          <input
            type="checkbox"
            checked={!!retry}
            onChange={(e) => onPatch({ retry: e.target.checked ? { ...DEFAULT_RETRY } : undefined })}
          />
          <span>Retry on failure</span>
        </label>
        {retry && <RetryEditor retry={retry} onChange={(r) => onPatch({ retry: r })} />}
      </fieldset>
    </aside>
  );
}

function RetryEditor({ retry, onChange }: { retry: RetryPolicy; onChange: (r: RetryPolicy) => void }) {
  const num = (key: keyof RetryPolicy, label: string, step = 1) => (
    <label className="field">
      <span>{label}</span>
      <input type="number" step={step} min={0} value={retry[key]} onChange={(e) => onChange({ ...retry, [key]: Number(e.target.value) })} />
    </label>
  );
  return (
    <div className="retry-grid">
      {num("max_attempts", "Max attempts")}
      {num("initial_delay_ms", "Initial delay (ms)")}
      {num("max_delay_ms", "Max delay (ms)")}
      {num("multiplier", "Multiplier", 0.1)}
      {num("jitter", "Jitter (0–1)", 0.05)}
    </div>
  );
}

function FieldEditor({ field, value, onChange }: { field: Field; value: unknown; onChange: (v: unknown) => void }) {
  const id = `f-${field.key}`;
  const label = (
    <span>
      {field.label}
      {field.required && <em aria-label="required"> *</em>}
    </span>
  );
  let control;
  switch (field.kind) {
    case "json":
      control = <JsonInput id={id} value={value} onChange={onChange} />;
      break;
    case "number":
      control = (
        <input
          id={id}
          type="number"
          value={typeof value === "number" ? value : ""}
          placeholder={field.placeholder}
          onChange={(e) => onChange(e.target.value === "" ? undefined : Number(e.target.value))}
        />
      );
      break;
    case "select":
      control = (
        <select id={id} value={typeof value === "string" ? value : ""} onChange={(e) => onChange(e.target.value)}>
          <option value="">default</option>
          {field.options?.map((o) => (
            <option key={o} value={o}>
              {o}
            </option>
          ))}
        </select>
      );
      break;
    case "bool":
      return (
        <label className="field inline">
          <input id={id} type="checkbox" checked={value === true} onChange={(e) => onChange(e.target.checked || undefined)} />
          {label}
        </label>
      );
    case "textarea":
      control = <textarea id={id} rows={4} value={typeof value === "string" ? value : ""} onChange={(e) => onChange(e.target.value)} />;
      break;
    default:
      control = (
        <input
          id={id}
          className={field.kind === "expr" ? "mono" : undefined}
          value={typeof value === "string" ? value : value === undefined ? "" : String(value)}
          placeholder={field.placeholder}
          spellCheck={false}
          onChange={(e) => onChange(e.target.value)}
        />
      );
  }
  return (
    <label className="field" htmlFor={id}>
      {label}
      {control}
      {field.help && <small>{field.help}</small>}
    </label>
  );
}

/** JSON textarea that keeps its draft text and only commits parseable values. */
function JsonInput({ id, value, onChange }: { id: string; value: unknown; onChange: (v: unknown) => void }) {
  const canon = value === undefined ? "" : JSON.stringify(value, null, 2);
  const [text, setText] = useState(canon);
  const [bad, setBad] = useState(false);
  useEffect(() => {
    // Reset when the selected node changes (value identity differs from the draft).
    setText((t) => {
      try {
        return t.trim() !== "" && JSON.stringify(JSON.parse(t)) === JSON.stringify(value) ? t : canon;
      } catch {
        return t;
      }
    });
  }, [canon, value]);
  return (
    <>
      <textarea
        id={id}
        className="mono"
        rows={4}
        spellCheck={false}
        aria-invalid={bad}
        value={text}
        onChange={(e) => {
          const t = e.target.value;
          setText(t);
          if (t.trim() === "") {
            setBad(false);
            onChange(undefined);
            return;
          }
          try {
            onChange(JSON.parse(t));
            setBad(false);
          } catch {
            setBad(true);
          }
        }}
      />
      {bad && <small className="err">Not valid JSON yet; the last valid value is kept.</small>}
    </>
  );
}

import type { NodeType } from "@protocol";
import { CATEGORIES, NODE_SPECS } from "../lib/schema";

export function Palette({ onAdd }: { onAdd: (t: NodeType) => void }) {
  return (
    <aside className="panel palette" aria-label="Node palette">
      {CATEGORIES.map((c) => (
        <section key={c.id}>
          <h4>{c.title}</h4>
          {NODE_SPECS.filter((s) => s.category === c.id).map((s) => (
            <button
              key={s.type}
              className={`pal-item cat-${s.category}`}
              draggable
              onDragStart={(e) => {
                e.dataTransfer.setData("application/synapse-node", s.type);
                e.dataTransfer.effectAllowed = "move";
              }}
              onClick={() => onAdd(s.type)}
              title={s.blurb}
            >
              <span className="pal-label">{s.label}</span>
              <span className="pal-blurb">{s.blurb}</span>
            </button>
          ))}
        </section>
      ))}
    </aside>
  );
}

import type { BoardRow, ViewLayout } from "../types";
import { groupRows, sortRows, formatWhen } from "../lib/view";

// The layout renderers. Each takes already-materialized BoardRows (title + the
// SQL-queryable axes: type / status / updated / position) — no `fields`, so a
// Documents board renders with no per-row fetch.

export function BoardLayout({ rows, layout }: { rows: BoardRow[]; layout: ViewLayout }) {
  const sorted = sortRows(rows, layout.sortBy, layout.sortDir);
  if (sorted.length === 0) {
    return <div className="board-empty">No rows match this view.</div>;
  }
  switch (layout.kind) {
    case "kanban":
      return <KanbanLayout rows={sorted} layout={layout} />;
    case "cards":
      return <CardsLayout rows={sorted} />;
    case "list":
      return <ListLayout rows={sorted} />;
    case "table":
    default:
      return <TableLayout rows={sorted} layout={layout} />;
  }
}

function Badge({ kind, value }: { kind: "type" | "status"; value?: string }) {
  if (!value) return null;
  return <span className={`badge badge-${kind}`}>{value}</span>;
}

function TableLayout({ rows, layout }: { rows: BoardRow[]; layout: ViewLayout }) {
  const cols = layout.columns ?? ["type", "status", "updated"];
  return (
    <div className="board-scroll">
      <table className="board-table">
        <thead>
          <tr>
            <th>Title</th>
            {cols.includes("type") && <th>Type</th>}
            {cols.includes("status") && <th>Status</th>}
            {cols.includes("updated") && <th>Updated</th>}
          </tr>
        </thead>
        <tbody>
          {rows.map((r) => (
            <tr key={r.id}>
              <td className="cell-title">{r.title}</td>
              {cols.includes("type") && <td>{r.type ?? ""}</td>}
              {cols.includes("status") && <td>{r.status ?? ""}</td>}
              {cols.includes("updated") && <td className="cell-dim">{formatWhen(r.updatedAt)}</td>}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function RowCard({ row }: { row: BoardRow }) {
  return (
    <div className="board-card">
      <div className="board-card-title">{row.title}</div>
      <div className="board-card-meta">
        <Badge kind="type" value={row.type} />
        <Badge kind="status" value={row.status} />
        {row.updatedAt ? <span className="cell-dim">{formatWhen(row.updatedAt)}</span> : null}
      </div>
    </div>
  );
}

function CardsLayout({ rows }: { rows: BoardRow[] }) {
  return (
    <div className="board-cards">
      {rows.map((r) => (
        <RowCard key={r.id} row={r} />
      ))}
    </div>
  );
}

function ListLayout({ rows }: { rows: BoardRow[] }) {
  return (
    <ul className="board-list">
      {rows.map((r) => (
        <li key={r.id} className="board-list-row">
          <span className="board-list-title">{r.title}</span>
          <Badge kind="status" value={r.status} />
        </li>
      ))}
    </ul>
  );
}

function KanbanLayout({ rows, layout }: { rows: BoardRow[]; layout: ViewLayout }) {
  const groups = groupRows(rows, layout.groupBy ?? "status");
  return (
    <div className="board-scroll">
      <div className="board-kanban">
        {groups.map((g) => (
          <div key={g.key} className="kanban-col">
            <div className="kanban-col-head">
              <span className="kanban-col-name">{g.key}</span>
              <span className="kanban-col-count">{g.rows.length}</span>
            </div>
            <div className="kanban-col-body">
              {g.rows.map((r) => (
                <RowCard key={r.id} row={r} />
              ))}
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}

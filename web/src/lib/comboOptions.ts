// The pure half of the shared combobox (components/Combobox): which rows match
// what was typed, and where the highlight goes. Kept out of the component so it
// can be tested without a DOM.

// filterOptions is the known rows whose id contains the query (case-insensitive),
// ids that START with it ahead of ones that merely contain it. An empty query
// keeps every row, in the order given.
//
// A query that matches nothing returns nothing — which is what lets a typed id
// the list does not know still be applied: with no row to highlight, Enter
// submits the text.
export function filterOptions<T extends { id: string }>(known: ReadonlyArray<T>, query: string): T[] {
  const q = query.trim().toLowerCase();
  if (!q) return [...known];
  const starts: T[] = [];
  const contains: T[] = [];
  for (const t of known) {
    const at = t.id.toLowerCase().indexOf(q);
    if (at === 0) starts.push(t);
    else if (at > 0) contains.push(t);
  }
  return [...starts, ...contains];
}

// moveActive steps the highlighted row for an arrow / Home / End key, wrapping
// at both ends. -1 means nothing is highlighted, which is the state after
// opening: Enter then submits what was typed instead of picking a row the
// operator never looked at.
export function moveActive(
  current: number,
  count: number,
  key: "ArrowDown" | "ArrowUp" | "Home" | "End",
): number {
  if (count <= 0) return -1;
  switch (key) {
    case "Home":
      return 0;
    case "End":
      return count - 1;
    case "ArrowDown":
      return current < 0 || current >= count - 1 ? 0 : current + 1;
    case "ArrowUp":
      return current <= 0 ? count - 1 : current - 1;
  }
}

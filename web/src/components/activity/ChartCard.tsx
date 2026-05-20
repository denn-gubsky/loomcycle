import type { ReactNode } from "react";

// ChartCard wraps one chart with a uniform header (title +
// current-value badge) and optional legend strip below the chart.
// The Activity Monitor renders one ChartCard per metric; this
// keeps all card layouts uniform without each chart having to
// duplicate header markup.

export interface LegendEntry {
  label: string;
  color: string;
}

export interface ChartCardProps {
  title: string;
  // The freshest scalar value (e.g. "342 MB", "12 runs"). Rendered
  // in the mono accent color. Optional — some cards (e.g. queue
  // depth) carry a derived "current" that's clearer in the chart
  // than as a single number.
  currentValue?: string;
  legend?: LegendEntry[];
  children: ReactNode;
}

export default function ChartCard({ title, currentValue, legend, children }: ChartCardProps) {
  return (
    <div className="chart-card">
      <div className="chart-card-header">
        <span className="chart-card-title">{title}</span>
        {currentValue !== undefined && (
          <span className="chart-card-current">{currentValue}</span>
        )}
      </div>
      <div className="chart-card-body">{children}</div>
      {legend && legend.length > 0 && (
        <div className="chart-card-legend">
          {legend.map((e) => (
            <span key={e.label} className="legend-entry">
              <span className="legend-swatch" style={{ background: e.color }} />
              {e.label}
            </span>
          ))}
        </div>
      )}
    </div>
  );
}

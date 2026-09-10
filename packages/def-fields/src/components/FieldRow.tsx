import type { FieldSpec } from "../types";
import { FieldControl } from "./FieldControl";

// One parameter: label · control · hint · inherit-vs-set state.
//
// This row is the unit both surfaces share. The grouped form and the folded list
// differ only in how rows are ARRANGED, never in how a parameter looks or what
// it explains — which is what keeps the two surfaces from drifting into two
// different mental models of the same def.

export interface FieldRowProps {
  spec: FieldSpec;
  /** undefined = unset (inherited). */
  value: unknown;
  onChange: (next: unknown) => void;
  /** Remove the key entirely, back to inherit. */
  onClear: () => void;
  disabled?: boolean;
  /** Compact = the folded list's denser one-line-per-parameter rhythm. */
  compact?: boolean;
}

export function FieldRow({ spec, value, onChange, onClear, disabled, compact }: FieldRowProps) {
  const set = value !== undefined;
  return (
    <div className={`lc-df-row${compact ? " lc-df-row-compact" : ""}${set ? " lc-df-row-set" : ""}`}>
      <div className="lc-df-row-head">
        <label className="lc-df-label" title={spec.key}>
          {spec.label}
          <code className="lc-df-key">{spec.key}</code>
        </label>
        {set ? (
          <button
            type="button"
            className="lc-df-reset"
            disabled={disabled}
            onClick={onClear}
            // The affordance names what clearing RESTORES, because "unset" on a
            // forked def does not mean "empty" — it means the parent's value
            // flows through again.
            title={spec.unsetMeans ? `Clear — ${spec.unsetMeans}` : "Clear (inherit)"}
          >
            clear
          </button>
        ) : (
          <span className="lc-df-inherited" title={spec.unsetMeans ?? "Not set on this def"}>
            inherited
          </span>
        )}
      </div>

      <div className="lc-df-control">
        <FieldControl
          spec={spec}
          value={value}
          onChange={onChange}
          disabled={disabled}
          renderChild={(child, childValue, childOnChange) => (
            <FieldRow
              key={child.key}
              spec={child}
              value={childValue}
              onChange={childOnChange}
              onClear={() => childOnChange(undefined)}
              disabled={disabled}
              compact={compact}
            />
          )}
        />
      </div>

      {/* The hint is ALWAYS rendered, never a hover-only tooltip: these
          parameters are the kind an operator meets once a year, and a tooltip
          they must discover is a hint that does not exist. */}
      <p className="lc-df-hint">{spec.hint}</p>
    </div>
  );
}

// MetricsDisabledEmpty — rendered when the server says metrics are
// off (either /healthz reports metrics_enabled: false on mount, or
// any /v1/_metrics request returns 503 with a typed-disabled
// result). Operator-facing copy explains how to turn it on.

export interface MetricsDisabledEmptyProps {
  enableHint: string;
}

export default function MetricsDisabledEmpty({ enableHint }: MetricsDisabledEmptyProps) {
  return (
    <div className="metrics-disabled">
      <div className="metrics-disabled-title">process sampler is off</div>
      <p className="metrics-disabled-body">
        loomcycle isn&apos;t collecting CPU / memory / agent-load samples on this
        deployment. To turn it on,{" "}
        <code>{enableHint}</code>.
      </p>
      <p className="metrics-disabled-body">
        For host-wide CPU + memory (the dashed second line on the CPU chart,
        and the system-memory card), also set{" "}
        <code>LOOMCYCLE_METRICS_COLLECT_SYSTEM=1</code>.
      </p>
    </div>
  );
}

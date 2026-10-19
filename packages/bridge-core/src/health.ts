// When the bridge last heard from each side, for GET /healthz. Times only:
// nothing here may carry a message, a name or a token.

export interface HealthSnapshot {
  ok: true;
  platform: string;
  started_at: string;
  last_inbound_at: string | null;
  last_outbound_at: string | null;
  last_loomcycle_at: string | null;
}

export class Health {
  private lastInbound?: number;
  private lastOutbound?: number;
  private lastLoomcycle?: number;
  private readonly startedAt: number;

  constructor(
    private readonly platform: string,
    private readonly now: () => number = Date.now,
  ) {
    this.startedAt = now();
  }

  inbound(): void {
    this.lastInbound = this.now();
  }

  outbound(): void {
    this.lastOutbound = this.now();
  }

  loomcycle(): void {
    this.lastLoomcycle = this.now();
  }

  snapshot(): HealthSnapshot {
    const iso = (t?: number) => (t === undefined ? null : new Date(t).toISOString());
    return {
      ok: true,
      platform: this.platform,
      started_at: new Date(this.startedAt).toISOString(),
      last_inbound_at: iso(this.lastInbound),
      last_outbound_at: iso(this.lastOutbound),
      last_loomcycle_at: iso(this.lastLoomcycle),
    };
  }
}

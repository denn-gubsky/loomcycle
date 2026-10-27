// A bounded set of recently seen ids, for dropping Slack's retries of an
// event already taken.

export class SeenSet {
  private readonly seen = new Map<string, number>();

  constructor(
    private readonly ttlMs: number,
    private readonly max: number,
    private readonly now: () => number = Date.now,
  ) {}

  /** add records id and reports whether it was new. */
  add(id: string): boolean {
    const t = this.now();
    const at = this.seen.get(id);
    if (at !== undefined && t - at < this.ttlMs) return false;
    this.seen.delete(id);
    this.seen.set(id, t);
    // Oldest first: a Map keeps insertion order.
    for (const [k, v] of this.seen) {
      if (this.seen.size <= this.max && t - v < this.ttlMs) break;
      this.seen.delete(k);
    }
    return true;
  }

  /** forget drops id, so a redelivery of it is taken again. */
  forget(id: string): void {
    this.seen.delete(id);
  }

  get size(): number {
    return this.seen.size;
  }
}

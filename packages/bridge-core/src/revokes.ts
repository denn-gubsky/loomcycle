// Revokes a gate bridge could not deliver, kept until loomcycle takes them.
//
// A platform says a person is gone (blocked the bot, deactivated, uninstalled
// the app) once, and redelivers only for a while: Slack for about an hour,
// Discord for minutes. A loomcycle outage longer than that used to lose the
// revoke, and the person stayed an active loomcycle user. Now a failed revoke
// is written here and the bridge retries it itself, with backoff, across
// restarts when the store is a file. Only subjects are kept: no token, no
// name, nothing a revoke does not need.

import { promises as fs } from "node:fs";

import { writePrivate } from "./store.js";

export interface PendingRevoke {
  /** The loomcycle subject, "<gate>:<sub>". */
  subject: string;
  /** Failed deliveries so far. */
  attempts: number;
  /** Epoch ms of the first failure. */
  firstFailedAt: number;
  /** Epoch ms before which the next attempt is not made. */
  nextAttemptAt: number;
}

export interface RevokeQueue {
  list(): Promise<PendingRevoke[]>;
  put(p: PendingRevoke): Promise<void>;
  delete(subject: string): Promise<void>;
}

export class MemoryRevokeQueue implements RevokeQueue {
  protected readonly items = new Map<string, PendingRevoke>();

  async list(): Promise<PendingRevoke[]> {
    return [...this.items.values()].map((p) => ({ ...p }));
  }

  async put(p: PendingRevoke): Promise<void> {
    this.items.set(p.subject, { ...p });
  }

  async delete(subject: string): Promise<void> {
    this.items.delete(subject);
  }
}

/** A JSON file beside the conversation store's, written the same way
 *  (writePrivate). */
export class FileRevokeQueue extends MemoryRevokeQueue {
  private loading?: Promise<void>;
  private writes: Promise<void> = Promise.resolve();

  constructor(private readonly file: string) {
    super();
  }

  // A file that fails to read (not one that is missing) stays failed: it is
  // never treated as empty, or the next write would replace it.
  private load(): Promise<void> {
    this.loading ??= this.read();
    return this.loading;
  }

  private async read(): Promise<void> {
    try {
      const data = JSON.parse(await fs.readFile(this.file, "utf8")) as PendingRevoke[];
      for (const p of data) {
        if (typeof p?.subject === "string" && p.subject) this.items.set(p.subject, p);
      }
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code !== "ENOENT") throw err;
    }
  }

  private persist(): Promise<void> {
    const snapshot = JSON.stringify([...this.items.values()], null, 2);
    this.writes = this.writes.then(async () => {
      await writePrivate(this.file, snapshot);
    });
    return this.writes;
  }

  override async list(): Promise<PendingRevoke[]> {
    await this.load();
    return super.list();
  }

  override async put(p: PendingRevoke): Promise<void> {
    await this.load();
    await super.put(p);
    await this.persist();
  }

  override async delete(subject: string): Promise<void> {
    await this.load();
    if (!this.items.has(subject)) return;
    await super.delete(subject);
    await this.persist();
  }
}

/** revokeQueueFromSpec keeps pending revokes where BRIDGE_STORE keeps
 *  conversations: "file:<path>" gives "<path>.revokes.json". */
export function revokeQueueFromSpec(spec: string): RevokeQueue {
  return spec.startsWith("file:") ? new FileRevokeQueue(`${spec.slice("file:".length)}.revokes.json`) : new MemoryRevokeQueue();
}

const MIN_BACKOFF_MS = 60_000;
const MAX_BACKOFF_MS = 60 * 60_000;

/** retryDelayMs is the wait after the n-th failed delivery: a minute,
 *  doubling, at most an hour. A revoke is never given up on. */
export function retryDelayMs(attempts: number): number {
  return Math.min(MAX_BACKOFF_MS, MIN_BACKOFF_MS * 2 ** Math.max(0, Math.min(attempts - 1, 10)));
}

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

import type { Logger } from "./ports.js";
import { removeStaleTemps, writePrivate } from "./store.js";

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
 *  (writePrivate). A revoke is never lost to the file: when the file cannot
 *  be read, a revoke put is kept in memory (and retried from there), the file
 *  is left as it is rather than overwritten, the fault is logged, and each
 *  later operation reads the file again, so a passing fault (EMFILE, EIO)
 *  does not stick. */
export class FileRevokeQueue extends MemoryRevokeQueue {
  private loaded = false;
  /** Subjects deleted before the file was read: a later read must not bring
   *  them back. */
  private readonly deleted = new Set<string>();
  private writes: Promise<void> = Promise.resolve();

  constructor(
    private readonly file: string,
    private readonly log?: Logger,
  ) {
    super();
  }

  /** load reads the file into memory once, merging what is already there
   *  (memory wins). It reports whether the file may be written. */
  private async load(): Promise<boolean> {
    if (this.loaded) return true;
    await removeStaleTemps(this.file);
    try {
      const data = JSON.parse(await fs.readFile(this.file, "utf8")) as PendingRevoke[];
      for (const p of data) {
        if (typeof p?.subject !== "string" || !p.subject || this.deleted.has(p.subject) || this.items.has(p.subject)) continue;
        this.items.set(p.subject, p);
      }
    } catch (err) {
      const code = (err as NodeJS.ErrnoException).code;
      if (code !== "ENOENT") {
        this.log?.error("the kept-revoke file could not be read; revokes are kept in memory and the file is left as it is", {
          error: code ?? (err as Error)?.name,
        });
        return false;
      }
    }
    this.loaded = true;
    this.deleted.clear();
    return true;
  }

  private persist(): Promise<void> {
    const snapshot = JSON.stringify([...this.items.values()], null, 2);
    // A failed write must not fail every later one: the chain goes on.
    const w = this.writes.catch(() => undefined).then(() => writePrivate(this.file, snapshot));
    this.writes = w;
    return w;
  }

  override async list(): Promise<PendingRevoke[]> {
    await this.load();
    return super.list();
  }

  override async put(p: PendingRevoke): Promise<void> {
    await super.put(p);
    if (await this.load()) await this.persist();
  }

  override async delete(subject: string): Promise<void> {
    if (!(await this.load())) {
      this.deleted.add(subject);
      await super.delete(subject);
      return;
    }
    if (!this.items.has(subject)) return;
    await super.delete(subject);
    await this.persist();
  }
}

/** revokeQueueFromSpec keeps pending revokes where BRIDGE_STORE keeps
 *  conversations: "file:<path>" gives "<path>.revokes.json". */
export function revokeQueueFromSpec(spec: string, log?: Logger): RevokeQueue {
  return spec.startsWith("file:") ? new FileRevokeQueue(`${spec.slice("file:".length)}.revokes.json`, log) : new MemoryRevokeQueue();
}

const MIN_BACKOFF_MS = 60_000;
const MAX_BACKOFF_MS = 60 * 60_000;

/** retryDelayMs is the wait after the n-th failed delivery: a minute,
 *  doubling, at most an hour. A revoke is never given up on. */
export function retryDelayMs(attempts: number): number {
  return Math.min(MAX_BACKOFF_MS, MIN_BACKOFF_MS * 2 ** Math.max(0, Math.min(attempts - 1, 10)));
}

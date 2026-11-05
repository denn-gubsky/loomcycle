// Where a bridge remembers which loomcycle session a conversation is in
// (RFC EN §5). Small on purpose: a Redis or Postgres store is these four
// methods.

import { randomUUID } from "node:crypto";
import { promises as fs } from "node:fs";
import path from "node:path";

import type { Scope } from "./identity.js";

/** `TRef` is the platform's reply reference, kept as plain JSON. */
export interface ConversationRecord<TRef = unknown> {
  conversationId: string;
  scope: Scope;
  /** Where to send replies: the latest inbound message's reference. */
  ref: TRef;
  /** The user who started the conversation: the one who answers its cards
   *  and may /new or /stop it. */
  ownerSubject: string;
  ownerName: string;
  /** The loomcycle user the runs act as: the person in a 1:1 chat, the
   *  conversation itself in a shared one (identity.ts). */
  runSubject: string;
  runName: string;
  /** The loomcycle session; a new run continues it with full history. */
  sessionId?: string;
  /** The live run, if any. */
  runId?: string;
  agentId?: string;
  /** Epoch ms of the last message. */
  lastActivityAt: number;
}

export interface ConversationStore<TRef = unknown> {
  get(conversationId: string): Promise<ConversationRecord<TRef> | undefined>;
  put(rec: ConversationRecord<TRef>): Promise<void>;
  delete(conversationId: string): Promise<void>;
  list(): Promise<ConversationRecord<TRef>[]>;
}

export class MemoryStore<TRef = unknown> implements ConversationStore<TRef> {
  protected readonly recs = new Map<string, ConversationRecord<TRef>>();

  async get(id: string): Promise<ConversationRecord<TRef> | undefined> {
    const r = this.recs.get(id);
    return r ? structuredClone(r) : undefined;
  }

  async put(rec: ConversationRecord<TRef>): Promise<void> {
    this.recs.set(rec.conversationId, structuredClone(rec));
  }

  async delete(id: string): Promise<void> {
    this.recs.delete(id);
  }

  async list(): Promise<ConversationRecord<TRef>[]> {
    return [...this.recs.values()].map((r) => structuredClone(r));
  }
}

/** A single JSON file, for one bridge instance. Writes go to a temp file and
 *  are renamed into place, so a crash never leaves a half-written file. */
export class FileStore<TRef = unknown> extends MemoryStore<TRef> {
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
      const data = JSON.parse(await fs.readFile(this.file, "utf8")) as ConversationRecord<TRef>[];
      for (const r of data) this.recs.set(r.conversationId, r);
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code !== "ENOENT") throw err;
    }
  }

  private persist(): Promise<void> {
    const snapshot = JSON.stringify([...this.recs.values()], null, 2);
    // Serialize writes: two overlapping renames could land the older snapshot last.
    this.writes = this.writes.then(async () => {
      await writePrivate(this.file, snapshot);
    });
    return this.writes;
  }

  override async get(id: string): Promise<ConversationRecord<TRef> | undefined> {
    await this.load();
    return super.get(id);
  }

  override async put(rec: ConversationRecord<TRef>): Promise<void> {
    await this.load();
    await super.put(rec);
    await this.persist();
  }

  override async delete(id: string): Promise<void> {
    await this.load();
    await super.delete(id);
    await this.persist();
  }

  override async list(): Promise<ConversationRecord<TRef>[]> {
    await this.load();
    return super.list();
  }
}

/** writePrivate replaces file with data, mode 0600, through a temp file of
 *  its own: a crash never leaves a half-written file, and a fixed temp name a
 *  crashed write left behind would keep that file's mode (writeFile sets the
 *  mode only when it creates the file). */
export async function writePrivate(file: string, data: string): Promise<void> {
  await fs.mkdir(path.dirname(file), { recursive: true });
  const tmp = `${file}.${process.pid}.${randomUUID()}.tmp`;
  try {
    await fs.writeFile(tmp, data, { mode: 0o600, flag: "wx" });
    await fs.rename(tmp, file);
  } catch (err) {
    await fs.rm(tmp, { force: true }).catch(() => undefined);
    throw err;
  }
}

export function storeFromSpec<TRef = unknown>(spec: string): ConversationStore<TRef> {
  return spec.startsWith("file:") ? new FileStore<TRef>(spec.slice("file:".length)) : new MemoryStore<TRef>();
}

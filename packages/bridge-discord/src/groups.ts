// A gate bridge's groups for a run identity (RFC FA §3.7d): what Discord
// knows, named as platform groups. loomcycle keeps them as "<gate>:<group>"
// external groups, which an operator maps (group_map) and binds ACLs to.

import { DiscordApiError, type DiscordApi } from "./api.js";
import { isSnowflake } from "./config.js";
import { guildGroup, roleGroup } from "./identity.js";
import type { DiscordRef } from "./port.js";

/** At most this many trusted servers are asked about a person's membership;
 *  past it the bridge asserts no groups for people (a channel's own user
 *  still gets its server's). */
export const MAX_GROUP_LOOKUPS = 20;
/** How long a person's looked-up groups are reused. */
export const GROUP_CACHE_MS = 10 * 60 * 1000;
const MAX_CACHED = 10_000;
/** Discord's JSON error code for a server the bot cannot see. */
const UNKNOWN_GUILD = 10004;

export interface GroupLookupOptions {
  api: Pick<DiscordApi, "guildMember">;
  /** The gate's name: a person is "<gate>:<user id>", a channel "<gate>:conv:<hash>". */
  gate: string;
  trustedGuilds: ReadonlySet<string>;
  now?: () => number;
}

export class GroupLookup {
  private readonly cache = new Map<string, { groups: string[]; until: number }>();
  private readonly now: () => number;

  constructor(private readonly opts: GroupLookupOptions) {
    this.now = opts.now ?? Date.now;
  }

  /** groupsFor: a channel's own user is in its server's group. A person (a
   *  DM) is in the group of each trusted server they are a member of, and in
   *  a group per role they hold there, looked up with the bot token. A lookup
   *  that fails, or more trusted servers than are worth asking about, asserts
   *  nothing, so loomcycle never removes a group on a guess. Cached per
   *  person for a while: a token is asked for rarely, but a restart asks for
   *  all of them at once. */
  async groupsFor(subject: string, ref: DiscordRef): Promise<string[] | undefined> {
    const prefix = `${this.opts.gate}:`;
    if (!subject.startsWith(prefix)) return undefined;
    const sub = subject.slice(prefix.length);
    // A ref stored before 0.3 has no server: assert nothing for it.
    if (sub.startsWith("conv:")) return ref.guildId ? [guildGroup(ref.guildId)] : undefined;
    if (!isSnowflake(sub)) return undefined;
    const trusted = this.opts.trustedGuilds;
    if (trusted.size === 0) return [];
    if (trusted.size > MAX_GROUP_LOOKUPS) return undefined;
    const now = this.now();
    const hit = this.cache.get(subject);
    if (hit && hit.until > now) return hit.groups;
    const out: string[] = [];
    for (const guild of [...trusted].sort()) {
      let member: { roles?: unknown } | undefined;
      try {
        member = await this.opts.api.guildMember(guild, sub);
      } catch (err) {
        // Not a member. A server the bot cannot see is also a 404, but says
        // nothing about the person.
        if (err instanceof DiscordApiError && err.status === 404 && err.code !== UNKNOWN_GUILD) continue;
        return undefined;
      }
      out.push(guildGroup(guild));
      const roles = Array.isArray(member?.roles) ? member.roles : [];
      for (const role of roles) if (isSnowflake(role)) out.push(roleGroup(role));
    }
    this.cache.delete(subject);
    this.cache.set(subject, { groups: out, until: now + GROUP_CACHE_MS });
    if (this.cache.size > MAX_CACHED) this.cache.delete(this.cache.keys().next().value!);
    return out;
  }

  /** forget drops a person's cached groups (they were revoked). */
  forget(subject: string): void {
    this.cache.delete(subject);
  }
}

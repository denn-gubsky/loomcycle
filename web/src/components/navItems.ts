import {
  Activity,
  Archive,
  Bell,
  BookOpen,
  Bot,
  Brain,
  CalendarClock,
  Camera,
  Coins,
  Eraser,
  FileText,
  FlaskConical,
  Gauge,
  HardDrive,
  HeartPulse,
  KeyRound,
  Layers,
  Library,
  Link2,
  ListChecks,
  ListTree,
  type LucideIcon,
  Network,
  Package,
  Play,
  Plug,
  Power,
  Radio,
  ScrollText,
  Server,
  ShieldCheck,
  Split,
  Ticket,
  Users,
  UsersRound,
  Workflow,
  Wrench,
} from "lucide-react";
import { canSee, grantOf, type Surfaces, type Visibility } from "../lib/visibility";

// Left-sidebar navigation model (RFC AS §4 — per-surface visibility class,
// replacing the old binary `adminOnly`):
//   "all"    — every authenticated role (run/runs: the principal-scoped workspace).
//   "tenant" — admin OR a substrate:tenant operator. The surface's reads are
//              tenant-scoped server-side (the operator sees only its own tenant)
//              and its writes are already reachable by substrate:tenant (RFC AF),
//              so the item lights up once it's visible.
//   "admin"  — super-admin only (operator plane / no per-tenant axis).
//
// A "tenant" item is ONLY assigned where the backing route gate actually admits
// substrate:tenant (requiredScopeFor → ScopeTenant or a tenantImplied scope):
// library (#575/#577), integrations + schedules (the *def/names + scheduledef
// def plane, #576 / isTenantConfinedDefPath), volumes (/v1/_volumes), paths
// (/v1/_path), interrupts (/v1/users/{id}/interrupts — runs:read, tenantImplied).
// memory (/v1/_memory/*) is "tenant": RFC BL gave memory rows a tenant_id, every
// handler sources the tenant from the principal (a tenant operator sees only its
// own tenant's memory), and RFC BV re-gated the routes to ScopeTenant — so the
// item lights up for a tenant operator. channels (/v1/_channels) is now "tenant"
// too: migration 0066 gave channel_messages/_cursors/channels a tenant_id, the
// handlers source the tenant from the principal (list filters by tenant; the
// cross-tenant `global` scope stays admin-only to create), and the routes were
// re-gated to ScopeTenant — closing the earlier admin-only carve-out.
// The type and the predicate live in ../lib/visibility — the Settings tabs use the
// same two, and a second copy is how the two surfaces drifted apart in the first place.
//
// `id` names the page in GET /v1/_me's capability summary (effective.surfaces):
// when the server answers for it, that answer decides and `vis` is only the
// fallback. The server's table (internal/api/http/nav_capabilities.go) must list
// every id here; a Go test reads this file to hold the two equal.
export interface NavItem {
  id: string;
  to: string;
  label: string;
  Icon: LucideIcon;
  vis: Visibility;
  // match is the path prefix that marks the item current when it differs from
  // `to` — a page whose link opens one of its own tabs (/library/agents) is
  // still the current item on its other tabs (/library/skills).
  match?: string;
}

// NAV_ITEMS is every page the left nav links to, one row per page. Where each
// one sits in the menu is NAV_GROUPS' business; this list is what the server's
// capability table is held equal to.
export const NAV_ITEMS: NavItem[] = [
  { id: "run", to: "/run", label: "new run", Icon: Play, vis: "all" },
  { id: "runs", to: "/agents", label: "all runs", Icon: ListTree, vis: "all" },
  { id: "interrupts", to: "/interrupts", label: "interrupts", Icon: Bell, vis: "tenant" },
  { id: "library", to: "/library/agents", label: "library", Icon: Library, vis: "tenant", match: "/library" },
  { id: "teams", to: "/teams", label: "teams", Icon: Workflow, vis: "tenant" },
  { id: "schedules", to: "/schedules", label: "schedules", Icon: CalendarClock, vis: "tenant" },
  // The Path tree is shown as "documents" — what people browse it for. The id
  // stays "paths": it is the key in the server's capability summary, and a Web UI
  // bundle can run against an older server that knows only that name.
  { id: "paths", to: "/documents", label: "documents", Icon: FileText, vis: "tenant" },
  { id: "memory", to: "/memory", label: "memory", Icon: Brain, vis: "tenant" },
  { id: "volumes", to: "/volumes/persistent", label: "volumes", Icon: HardDrive, vis: "tenant", match: "/volumes" },
  // ScopeTenant on the server: /v1/_ontology, /v1/_credentialdef
  // (isTenantConfinedDefPath), /v1/_limits, /v1/_routing, /v1/_retention and
  // /v1/_erasure.
  { id: "ontology", to: "/ontology", label: "ontology", Icon: Network, vis: "tenant" },
  // The statement review queue (RFC FS). The server's answer decides: it shows
  // the page to an operator, or to a member an access-control list grants review.
  { id: "review", to: "/review", label: "review", Icon: ListChecks, vis: "tenant" },
  { id: "integrations", to: "/integrations/webhooks", label: "integrations", Icon: Plug, vis: "tenant", match: "/integrations" },
  { id: "channels", to: "/channels", label: "channels", Icon: Radio, vis: "tenant" },
  // The tenant's credential authoring — distinct from the account menu's My
  // credentials, which is a viewer's own.
  { id: "credentials", to: "/credentials", label: "tenant credentials", Icon: KeyRound, vis: "tenant" },
  // Access: who the tenant's people are and what they may do. Each section is a
  // page of /access, gated by the route it loads first (users the user list,
  // groups/policies/bindings/simulator their own ACL routes, tokens operator
  // token minting) — so an operator an ACL denies policies still manages users.
  { id: "users", to: "/access/users", label: "users", Icon: Users, vis: "tenant" },
  { id: "groups", to: "/access/groups", label: "groups", Icon: UsersRound, vis: "tenant" },
  { id: "policies", to: "/access/policies", label: "policies", Icon: ShieldCheck, vis: "tenant" },
  { id: "bindings", to: "/access/bindings", label: "bindings", Icon: Link2, vis: "tenant" },
  { id: "simulator", to: "/access/simulator", label: "simulator", Icon: FlaskConical, vis: "tenant" },
  { id: "tokens", to: "/access/tokens", label: "tokens", Icon: Ticket, vis: "admin" },
  // usage: token/cost report. Tenant-visible (RFC AV): GET /v1/_usage is
  // ScopeTenant-gated and the handler tenant-scopes the aggregation (a tenant
  // operator sees only its own tenant's spend; admin sees all + ?tenant=).
  { id: "usage", to: "/usage", label: "usage", Icon: Coins, vis: "tenant" },
  { id: "limits", to: "/limits", label: "limits", Icon: Gauge, vis: "tenant" },
  { id: "routing", to: "/routing", label: "routing", Icon: Split, vis: "tenant" },
  // audit is tenant-visible (RFC AS): handleListEvents tenant-scopes the result
  // via the event's owning session, so a tenant sees only its own events.
  { id: "audit", to: "/audit", label: "audit", Icon: ScrollText, vis: "tenant" },
  { id: "retention", to: "/retention", label: "retention", Icon: Archive, vis: "tenant" },
  { id: "erasure", to: "/erasure", label: "erasure", Icon: Eraser, vis: "tenant" },
  // ScopeAdmin: the operator plane. Activity, presets, runtime, lanes and health
  // fall through to the /v1/_* catch-all; repair-tenant (maintenance) is
  // explicitly admin because it rewrites rows across every scope in one statement.
  { id: "activity", to: "/activity", label: "activity", Icon: Activity, vis: "admin" },
  { id: "snapshots", to: "/snapshots", label: "snapshots", Icon: Camera, vis: "admin" },
  { id: "runtime", to: "/runtime", label: "runtime", Icon: Power, vis: "admin" },
  { id: "lanes", to: "/lanes", label: "lanes", Icon: Layers, vis: "admin" },
  { id: "presets", to: "/presets", label: "presets", Icon: Package, vis: "admin" },
  { id: "maintenance", to: "/maintenance", label: "maintenance", Icon: Wrench, vis: "admin" },
  { id: "health", to: "/health", label: "health", Icon: HeartPulse, vis: "admin" },
  // NOTE: the viewer's own pages — Your account, My credentials — are in the top
  // bar's account menu (ACCOUNT_LINKS in ../lib/accountMenu), not here.
];

// A zone is a band of the menu: the workspace people use every day, and the
// administration of the tenant or the runtime.
export type NavZone = "workspace" | "admin";

export const ZONE_LABELS: Record<NavZone, string> = {
  workspace: "workspace",
  admin: "administration",
};

// NavGroup is one collapsible heading of the menu and the pages under it, named
// by NAV_ITEMS id. `key` (not `id`) because it names no page: the Go test reads
// every `id:` of NAV_ITEMS as a page, and a group is not one.
export interface NavGroup {
  key: string;
  label: string;
  Icon: LucideIcon;
  zone: NavZone;
  items: string[];
}

// NAV_GROUPS is the menu's hierarchy, in order. Every NAV_ITEMS page sits in
// exactly one group (navItems.test.ts holds that).
export const NAV_GROUPS: NavGroup[] = [
  { key: "g-runs", label: "runs", Icon: Play, zone: "workspace", items: ["run", "runs", "interrupts"] },
  { key: "g-agents", label: "agents", Icon: Bot, zone: "workspace", items: ["library", "teams", "schedules"] },
  {
    key: "g-knowledge",
    label: "knowledge",
    Icon: BookOpen,
    zone: "workspace",
    items: ["paths", "memory", "volumes", "ontology", "review"],
  },
  {
    key: "g-connections",
    label: "connections",
    Icon: Plug,
    zone: "workspace",
    items: ["integrations", "channels", "credentials"],
  },
  {
    key: "g-access",
    label: "access",
    Icon: ShieldCheck,
    zone: "admin",
    items: ["users", "groups", "policies", "bindings", "simulator", "tokens"],
  },
  { key: "g-cost", label: "cost & models", Icon: Coins, zone: "admin", items: ["usage", "limits", "routing"] },
  { key: "g-compliance", label: "compliance", Icon: ScrollText, zone: "admin", items: ["audit", "retention", "erasure"] },
  {
    key: "g-system",
    label: "system",
    Icon: Server,
    zone: "admin",
    items: ["activity", "snapshots", "runtime", "lanes", "presets", "maintenance", "health"],
  },
];

export interface VisibleGroup {
  group: NavGroup;
  items: NavItem[];
}

export interface VisibleZone {
  zone: NavZone;
  groups: VisibleGroup[];
}

// visibleNavTree is the menu for a viewer: each zone with the groups that keep
// at least one page the viewer may open, each group with only those pages — the
// server's answer per page when it gave one, the role rule otherwise. A group,
// or a zone, with nothing left is dropped rather than shown empty.
export function visibleNavTree(
  isAdmin: boolean,
  hasTenantScope: boolean,
  surfaces: Surfaces | undefined,
): VisibleZone[] {
  const byId = new Map(NAV_ITEMS.map((it) => [it.id, it]));
  const zones: VisibleZone[] = [];
  for (const group of NAV_GROUPS) {
    const items = group.items
      .map((id) => byId.get(id))
      .filter((it): it is NavItem => !!it && canSee(it.vis, isAdmin, hasTenantScope, grantOf(surfaces, it.id)));
    if (items.length === 0) continue;
    let zone = zones.find((z) => z.zone === group.zone);
    if (!zone) {
      zone = { zone: group.zone, groups: [] };
      zones.push(zone);
    }
    zone.groups.push({ group, items });
  }
  return zones;
}

// visibleNavItems is the same menu flattened to its pages, in menu order.
export function visibleNavItems(
  isAdmin: boolean,
  hasTenantScope: boolean,
  surfaces: Surfaces | undefined,
): NavItem[] {
  return visibleNavTree(isAdmin, hasTenantScope, surfaces).flatMap((z) => z.groups.flatMap((g) => g.items));
}

// isNavItemCurrent says whether the page at pathname is this item: its link, or
// anything beneath its match prefix.
export function isNavItemCurrent(item: NavItem, pathname: string): boolean {
  const base = item.match ?? item.to;
  return pathname === item.to || pathname === base || pathname.startsWith(base + "/");
}

// visibleGroupItems is one group's pages a viewer may open, in menu order — what
// a page holding the whole group (the /access sections) offers that viewer.
export function visibleGroupItems(
  groupKey: string,
  isAdmin: boolean,
  hasTenantScope: boolean,
  surfaces: Surfaces | undefined,
): NavItem[] {
  return (
    visibleNavTree(isAdmin, hasTenantScope, surfaces)
      .flatMap((z) => z.groups)
      .find((g) => g.group.key === groupKey)?.items ?? []
  );
}

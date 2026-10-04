import {
  Activity,
  Bell,
  Brain,
  CalendarClock,
  Camera,
  Coins,
  FolderTree,
  HardDrive,
  KeyRound,
  Library,
  ListTree,
  type LucideIcon,
  Play,
  Plug,
  Radio,
  ScrollText,
  Workflow,
} from "lucide-react";
import { type Visibility } from "../lib/visibility";

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
export interface NavItem {
  to: string;
  label: string;
  Icon: LucideIcon;
  vis: Visibility;
}
export const NAV_ITEMS: NavItem[] = [
  { to: "/run", label: "run", Icon: Play, vis: "all" },
  { to: "/agents", label: "runs", Icon: ListTree, vis: "all" },
  // RFC CN — every login (incl. an isolated substrate:user user) can self-serve
  // its OWN scope=user API tokens here; the operator Settings → Credentials tab
  // (tenant authoring) stays separate.
  { to: "/my-credentials", label: "credentials", Icon: KeyRound, vis: "all" },
  { to: "/library/agents", label: "library", Icon: Library, vis: "tenant" },
  { to: "/integrations/webhooks", label: "integrations", Icon: Plug, vis: "tenant" },
  { to: "/volumes/persistent", label: "volumes", Icon: HardDrive, vis: "tenant" },
  { to: "/paths", label: "paths", Icon: FolderTree, vis: "tenant" },
  { to: "/channels", label: "channels", Icon: Radio, vis: "tenant" },
  { to: "/schedules", label: "schedules", Icon: CalendarClock, vis: "tenant" },
  { to: "/teams", label: "teams", Icon: Workflow, vis: "tenant" },
  { to: "/interrupts", label: "interrupts", Icon: Bell, vis: "tenant" },
  { to: "/memory", label: "memory", Icon: Brain, vis: "tenant" },
  { to: "/snapshots", label: "snapshots", Icon: Camera, vis: "admin" },
  // audit is tenant-visible (RFC AS): handleListEvents tenant-scopes the result
  // via the event's owning session, so a tenant sees only its own events.
  { to: "/audit", label: "audit", Icon: ScrollText, vis: "tenant" },
  // usage: token/cost report. Tenant-visible (RFC AV): GET /v1/_usage is
  // ScopeTenant-gated and the handler tenant-scopes the aggregation (a tenant
  // operator sees only its own tenant's spend; admin sees all + ?tenant=).
  { to: "/usage", label: "usage", Icon: Coins, vis: "tenant" },
  { to: "/activity", label: "activity", Icon: Activity, vis: "admin" },
  // NOTE: routing + limits moved OUT of the main nav INTO Settings tabs (the
  // /routing + /limits routes still exist for deep links, but SettingsView is
  // now the primary surface — see SettingsView's routing/limits tabs).
  // users (/users) is likewise reached from the top bar's settings menu, not from
  // here — see MENU_LINKS in ../lib/settingsSections.
];

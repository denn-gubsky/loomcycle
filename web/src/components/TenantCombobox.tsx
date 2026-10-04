import { useCallback, useEffect, useState } from "react";
import { listTenants } from "../api";
import { filterTenants, normaliseTenants, type TenantOption } from "../lib/tenantOptions";
import Combobox, { type ComboRow } from "./Combobox";

// TenantCombobox is the admin tenant-focus field: a text input the admin can type
// any tenant id into, plus a drop-down of the tenants the directory knows about.
//
// The list is not the set of tenants — GET /v1/_tenants is derived from runs, so
// a tenant that has never run anything is absent from it — which is why typing
// stays first-class (see Combobox).
//
// It degrades to the plain input it replaced whenever there is no list to offer:
// the viewer is not an admin (`enabled` false — the endpoint is admin-only and the
// list of tenants is itself cross-tenant information), or the call failed.
interface Props {
  id?: string;
  value: string;
  // Every keystroke. The owner decides what a half-typed id means.
  onChange: (text: string) => void;
  // A row was chosen from the list; "" is the "all tenants" row.
  onPick: (tenant: string) => void;
  enabled: boolean;
  placeholder?: string;
  title?: string;
}

function toRow(t: TenantOption): ComboRow {
  if (t.id === "") return { id: "", label: "all tenants", quiet: true };
  return {
    id: t.id,
    label: t.id,
    hint: t.runs === undefined ? undefined : `${t.runs} ${t.runs === 1 ? "run" : "runs"}`,
  };
}

export default function TenantCombobox({ enabled, ...field }: Props) {
  // null = no list to offer (not loaded yet, or the load failed) → plain input.
  const [known, setKnown] = useState<TenantOption[] | null>(null);

  const load = useCallback(() => {
    if (!enabled) return;
    listTenants()
      .then((r) => setKnown(normaliseTenants(r.tenants)))
      .catch(() => setKnown(null));
  }, [enabled]);

  // Load once up front so the toggle is there before the field is first touched;
  // each focus refreshes it — tenants appear as they start their first run, and a
  // list fetched once at page load would never show them.
  useEffect(load, [load]);

  return (
    <Combobox
      {...field}
      rows={enabled && known !== null ? (q) => filterTenants(known, q).map(toRow) : null}
      onFocus={load}
      listLabel="Known tenants"
      toggleLabel="Show known tenants"
    />
  );
}

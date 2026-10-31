import { useCallback, useEffect, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useUserId } from "../components/Layout";
import { ontologistRunHref, ontologyEditHref } from "../lib/ontologyEditHref";
import {
  buildRelationTree,
  classDirectiveNotes,
  conceptSummary,
  instanceStateLabel,
  factsUnderOlderVocabulary,
  ontologyTabs,
  pickOntologyTab,
  relationSummary,
  typedFieldLabel,
  type RelationNode,
} from "../lib/ontologySections";
import {
  OntologyProposal,
  OntologyResponse,
  OntologyTerm,
  adoptOntologyType,
  getOntology,
  resolveOntologyProposal,
  setOntologyStatus,
} from "../api";

// The ontology page (/ontology): the tenant ontology's confirm gate and the
// layered result, one section per tab.

// ─── Ontology tree rendering (RFC BZ P4) ─────────────────────────────────────

// OntologyNode is a term with its subclasses attached.
type OntologyNode = OntologyTerm & { children: OntologyNode[] };

// buildOntologyTree assembles the parent/child tree the panel renders.
//
// An ORPHAN — a term whose named parent is absent — is rendered at the root rather
// than dropped, matching what the prompt renderer does with the same input. A type
// missing from this panel is a type the operator cannot see is in force, which is the
// silent-loss failure the whole feature exists to end.
function buildOntologyTree(terms: OntologyTerm[]): OntologyNode[] {
  const byName = new Map<string, OntologyNode>();
  terms.forEach((t) => byName.set(t.name, { ...t, children: [] }));
  const roots: OntologyNode[] = [];
  byName.forEach((n) => {
    const parent = n.parent ? byName.get(n.parent) : undefined;
    if (parent && parent !== n) parent.children.push(n);
    else roots.push(n);
  });
  return roots;
}

// ontologyFieldSummary describes a type's fields the way an operator needs to judge it:
// how many there are in total, which the type declared, and which it inherited.
function OntologyFieldSummary({ term, isLeaf }: { term: OntologyNode; isLeaf: boolean }) {
  const declared = term.fields ?? [];
  const inherited = term.inherited ?? [];
  const total = declared.length + inherited.length;
  // THE PHANTOM TEST IS ON DECLARED FIELDS, NOT ON THE TOTAL, and getting this wrong
  // would have missed the exact case worth warning about. The scenario is an operator
  // adding a section chunk for readability — "Notes on naming" under `project` — which
  // silently becomes a subclass. It INHERITS its parent's fields, so a total-based test
  // sees a well-formed type and says nothing. A leaf that declares nothing of its own
  // is a folder, not a class.
  //
  // A node with CHILDREN that declares nothing is different and fine: that is a
  // deliberate abstract grouping type (`work item` over `project` and `task`).
  const phantom = declared.length === 0 && isLeaf;
  // Typed classes (RFC FO) show each field's type and flags; an untyped field reads
  // as its bare name, so a section-less ontology looks exactly as it did.
  const declaredLabels = term.field_specs?.length ? term.field_specs.map(typedFieldLabel) : declared;
  const inheritedLabels = term.inherited_specs?.length
    ? term.inherited_specs.map(typedFieldLabel)
    : inherited;
  const directives = classDirectiveNotes(term);
  // Typed labels carry commas of their own ("enum(a, b)", "required, identity"), so
  // they are separated by semicolons; plain names keep the comma they always had.
  const sep = [...declaredLabels, ...inheritedLabels].some((l) => l.includes(":")) ? "; " : ", ";
  return (
    <span className="settings-muted">
      {" "}
      {term.document_class && <span className="settings-flash">document class · </span>}
      {total === 0 ? "no fields" : `${total} field${total === 1 ? "" : "s"}`}
      {declaredLabels.length > 0 && <> — {declaredLabels.join(sep)}</>}
      {inheritedLabels.length > 0 && (
        <span className="ontology-inherited">
          {" "}
          · inherited: {inheritedLabels.join(sep)}
        </span>
      )}
      {directives.length > 0 && <> · {directives.join(" · ")}</>}
      {(term.issues ?? []).map((iss) => (
        <span className="ontology-warn" key={iss}>
          {" "}
          · {iss}
        </span>
      ))}
      {phantom && (
        <span className="ontology-warn">
          {" "}
          · declares no fields of its own — a section heading rather than a type?
        </span>
      )}
      {term.name_issue && (
        <span className="ontology-warn">
          {" "}
          · name {term.name_issue}
        </span>
      )}
    </span>
  );
}

// OntologyTermTree renders the nodes, recursing into subclasses.
//
// Depth-bounded even though the chunk tree cannot cycle: a malformed term list must not
// be able to hang the operator's browser on the one page they would use to fix it.
function OntologyTermTree({
  nodes,
  depth = 0,
  markSource = false,
}: {
  nodes: OntologyNode[];
  depth?: number;
  markSource?: boolean;
}) {
  if (nodes.length === 0 || depth > 8) return null;
  return (
    <ul className="ontology-tree">
      {nodes.map((n) => (
        <li key={n.name}>
          <code>{n.name}</code>
          {markSource && n.source === "tenant" && (
            <span className="settings-flash"> yours</span>
          )}
          <OntologyFieldSummary term={n} isLeaf={n.children.length === 0} />
          <OntologyTermTree nodes={n.children} depth={depth + 1} markSource={markSource} />
        </li>
      ))}
    </ul>
  );
}

// OntologyRelationTree renders relation kinds, recursing into sub-kinds. Depth-bounded
// like the class tree, for the same reason.
function OntologyRelationTree({
  nodes,
  depth = 0,
  markSource = false,
}: {
  nodes: RelationNode[];
  depth?: number;
  markSource?: boolean;
}) {
  if (nodes.length === 0 || depth > 8) return null;
  return (
    <ul className="ontology-tree">
      {nodes.map((n) => {
        const summary = relationSummary(n);
        return (
          <li key={n.name}>
            <code>{n.name}</code>
            {markSource && n.source === "tenant" && <span className="settings-flash"> yours</span>}
            {markSource && n.source === "reserved" && (
              <span className="settings-muted"> reserved</span>
            )}
            {summary && <span className="settings-muted"> {summary}</span>}
            {[...(n.issues ?? []), ...(n.name_issue ? [n.name_issue] : [])].map((iss) => (
              <span className="ontology-warn" key={iss}>
                {" "}
                · {iss}
              </span>
            ))}
            <OntologyRelationTree nodes={n.children} depth={depth + 1} markSource={markSource} />
          </li>
        );
      })}
    </ul>
  );
}

// OntologyRelationsDetail is the Relations section's half of the panel (RFC FO P1b): what
// this deployment declares beside what is in force — the standard kinds come into force
// with the section, but only the deployment's own (declared or adopted) are listed to
// agents, which is why adopting is offered here.
function OntologyRelationsDetail({
  state,
  busy,
  onAdopt,
}: {
  state: OntologyResponse;
  busy: boolean;
  onAdopt: (name: string) => void;
}) {
  const mine = state.relations ?? [];
  const inForce = state.effective_relations ?? [];
  return (
    <>
      <div className="settings-row">
        <table className="settings-table ontology-compare">
          <thead>
            <tr>
              <th>relation kinds this deployment defines</th>
              <th>relation kinds in force now</th>
            </tr>
          </thead>
          <tbody>
            <tr>
              <td>
                {mine.length === 0 ? (
                  <span className="settings-muted">none yet — the standard kinds still apply</span>
                ) : (
                  <OntologyRelationTree nodes={buildRelationTree(mine)} />
                )}
              </td>
              <td>
                {inForce.length === 0 ? (
                  <span className="settings-muted">none — confirm the ontology to apply them</span>
                ) : (
                  <OntologyRelationTree nodes={buildRelationTree(inForce)} markSource />
                )}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      {(state.adoptable_relations ?? []).length > 0 && (
        <p className="settings-help">
          <strong>List a standard kind to agents:</strong> standard kinds are valid without
          this, but agents are only told about the kinds your document declares. Adopting
          copies one in, frozen — edit it freely afterwards.
          <span className="ontology-adopt-row">
            {(state.adoptable_relations ?? []).map((name) => (
              <button
                key={name}
                type="button"
                className="ghost-btn"
                disabled={busy}
                onClick={() => onAdopt(name)}
                title={`Copy the standard relation kind "${name}" into your Relations section`}
              >
                adopt <code>{name}</code>
              </button>
            ))}
          </span>
        </p>
      )}
      {(state.undated_statements ?? 0) > 0 && (
        <p className="settings-help">
          {state.undated_statements} statement(s) of a dated relation kind carry no date.
        </p>
      )}
    </>
  );
}

// OntologyInstancesDetail is the Instances section's half of the panel (RFC FO P1c-2):
// each instance with its class, what a confirm does (or did) for it, and its registry key
// — the key the memory tier files facts about it under. Subjects whose instance was
// removed are listed as kept, because removal never erases.
function OntologyInstancesDetail({ state }: { state: OntologyResponse }) {
  const rep = state.instances;
  const did = state.instance_adoption;
  return (
    <div className="settings-row">
      <div>
        <strong>Instances</strong>
        <span className="settings-muted">
          {" "}
          — real organisations, units and posts; a confirm adopts them into memory.
        </span>
        {did && (
          <p className="settings-flash">
            This confirm adopted {did.adopted}, matched {did.linked} to existing subjects, renamed{" "}
            {did.renamed}, and wrote {did.statements} statement(s).
          </p>
        )}
        <ul className="ontology-tree">
          {(rep?.instances ?? []).map((i) => (
            <li key={i.name}>
              <code>{i.name}</code>
              {i.class && <span className="settings-muted"> · instance of {i.class}</span>}
              <span className={i.state === "error" ? "ontology-warn" : "settings-muted"}>
                {" "}
                · {instanceStateLabel(i)}
              </span>
              {i.key && <span className="settings-muted"> · {i.key}</span>}
            </li>
          ))}
        </ul>
        {(rep?.orphaned ?? []).length > 0 && (
          <p className="settings-help">
            No longer declared, kept in memory (delete a subject through erasure, not here):{" "}
            {(rep?.orphaned ?? []).join(", ")}
          </p>
        )}
      </div>
    </div>
  );
}

// OntologySchemesDetail is the Schemes section's half of the panel (RFC FO P1c): each
// scheme's concepts as a tree — a concept's tag is its identity in every scope, which is
// how a document is classified under it — and, when a concept chunk lacks its tag, the
// note that confirming again applies it.
function OntologySchemesDetail({ state }: { state: OntologyResponse }) {
  const stale = state.scheme_mirror_stale ?? [];
  return (
    <div className="settings-row">
      <div>
        <strong>Concept schemes</strong>
        <span className="settings-muted">
          {" "}
          — a document is classified under a concept by carrying its tag.
        </span>
        {(state.schemes ?? []).map((sc) => (
          <div key={sc.name}>
            <code>{sc.name}</code>
            {sc.ordered && <span className="settings-muted"> ordered</span>}
            {sc.render && <span className="settings-flash"> listed to agents</span>}
            {(sc.issues ?? []).map((iss) => (
              <span className="ontology-warn" key={iss}>
                {" "}
                · {iss}
              </span>
            ))}
            <ul className="ontology-tree">
              {(sc.concepts ?? []).map((c) => (
                <li key={c.tag} style={{ marginLeft: `${(c.path.length - 1) * 1.2}em` }}>
                  <code>{c.name}</code>
                  <span className="settings-muted"> {conceptSummary(c)}</span>
                  {(c.issues ?? []).map((iss) => (
                    <span className="ontology-warn" key={iss}>
                      {" "}
                      · {iss}
                    </span>
                  ))}
                </li>
              ))}
            </ul>
          </div>
        ))}
        {stale.length > 0 && (
          <p className="settings-help">
            {stale.length} concept(s) do not carry their tag yet — confirm the ontology again to
            apply them.
          </p>
        )}
      </div>
    </div>
  );
}

// OntologyClassesTab is the classes a run extracts against: what this deployment
// defines beside what is in force — the gap between the two columns is the only
// visible evidence that a draft is inert — and the standard types it can adopt.
function OntologyClassesTab({
  state,
  busy,
  onAdopt,
}: {
  state: OntologyResponse;
  busy: boolean;
  onAdopt: (name: string) => void;
}) {
  const tenantTerms = state.terms ?? [];
  return (
    <>
      {state.confirmed && tenantTerms.length === 0 && (
        <p className="settings-help">
          Confirmed, but no classes were found in the document. Each class needs its own
          heading, with field names in backticks on the bullets beneath it — anything else is
          skipped, so a differently-formatted document confirms to nothing.
        </p>
      )}
      {/* ADOPT — the standard types this document does not declare. Offered here
          because the documented way to subclass one is to declare it yourself first,
          which otherwise means retyping its field names off the column on the right. */}
      {(state.adoptable ?? []).length > 0 && (
        <p className="settings-help">
          <strong>Extend a standard type:</strong> adopting one copies it into your document
          with its fields, so you can add to it or nest subtypes under it. Your copy then
          overrides the standard one, wholesale — a field a later loomcycle release adds to it
          will not reach your copy.
          <span className="ontology-adopt-row">
            {(state.adoptable ?? []).map((name) => (
              <button
                key={name}
                type="button"
                className="ghost-btn"
                disabled={busy}
                onClick={() => onAdopt(name)}
                title={`Copy the standard "${name}" into your document so you can extend or subclass it`}
              >
                adopt <code>{name}</code>
              </button>
            ))}
          </span>
        </p>
      )}
      <div className="settings-row">
        <table className="settings-table ontology-compare">
          <thead>
            <tr>
              <th>this deployment defines</th>
              <th>in force now</th>
            </tr>
          </thead>
          <tbody>
            <tr>
              <td>
                {tenantTerms.length === 0 ? (
                  <span className="settings-muted">none yet</span>
                ) : (
                  <OntologyTermTree nodes={buildOntologyTree(tenantTerms)} />
                )}
              </td>
              <td>
                <OntologyTermTree nodes={buildOntologyTree(state.effective ?? [])} markSource />
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </>
  );
}

// OntologyDocumentClassesTab: the classes that type a document rather than a fact —
// kept apart from Classes because they never reach an agent's prompt.
function OntologyDocumentClassesTab({ state }: { state: OntologyResponse }) {
  return (
    <div className="settings-row">
      <div>
        <span className="settings-muted">
          Govern a document's type: its title and identifier patterns, the grammars that read
          its citations and anchors, and the work key editions share. Never listed to agents
          as entity types.
        </span>
        <OntologyTermTree nodes={buildOntologyTree(state.document_classes ?? [])} markSource />
      </div>
    </div>
  );
}

// OntologyGrammarsTab lists each grammar with what it does, and its rules on demand:
// a grammar that misreads a citation is debugged by reading its patterns, and the
// document view shows them only one chunk at a time.
function OntologyGrammarsTab({ state }: { state: OntologyResponse }) {
  return (
    <div className="settings-row">
      <div>
        <span className="settings-muted">
          Patterns that read citations in a document's text and key its parts. A class uses
          one by naming it in <code>@citation_grammar</code> or <code>@anchor_grammar</code>; a
          grammar that cannot be used is disabled and reported, and the class stays in force.
        </span>
        <ul className="ontology-tree">
          {(state.grammars ?? []).map((g) => (
            <li key={g.name}>
              <code>{g.name}</code>
              {g.class && <span className="settings-muted"> under {g.class}</span>}
              {g.issue ? (
                <span className="ontology-warn"> · disabled: {g.issue}</span>
              ) : (
                <span className="settings-muted">
                  {" "}
                  · {g.patterns?.length ?? 0} pattern(s)
                  {g.resolve && <> → {g.resolve}</>}
                  {(g.scopes?.length ?? 0) > 0 && <> · {g.scopes?.length} scope(s)</>}
                  {(g.anchors?.length ?? 0) > 0 && <> · {g.anchors?.length} anchor rule(s)</>}
                </span>
              )}
              {((g.patterns?.length ?? 0) > 0 ||
                (g.scopes?.length ?? 0) > 0 ||
                (g.anchors?.length ?? 0) > 0) && (
                <details className="settings-help">
                  <summary>rules</summary>
                  <pre className="ontology-evidence ontology-rules">
                    {[
                      ...(g.patterns ?? []).map((x) => `pattern: ${x}`),
                      ...(g.resolve ? [`resolve: ${g.resolve}`] : []),
                      ...(g.scopes ?? []).map((r) => `scope: ${r.pattern} => ${r.templates.join(" | ")}`),
                      ...(g.anchors ?? []).map((r) => `anchor: ${r.pattern} => ${r.templates.join(" | ")}`),
                    ].join("\n")}
                  </pre>
                </details>
              )}
            </li>
          ))}
        </ul>
      </div>
    </div>
  );
}

// OntologyHealthTab: what in the tenant's data disagrees with the ontology. Nothing
// here is an error in the document; each is a gap between the vocabulary and the facts
// already written under it.
function OntologyHealthTab({ state }: { state: OntologyResponse }) {
  const versions = factsUnderOlderVocabulary(state);
  return (
    <>
      {(state.undeclared_types ?? []).length > 0 && (
        <p className="settings-help">
          <strong>Types in use that no class declares</strong> (tenant scope):{" "}
          {(state.undeclared_types ?? []).map((t) => (
            <span key={t.type}>
              <code>{t.type}</code> <span className="settings-muted">×{t.count}</span>{" "}
            </span>
          ))}
          — declare them as classes, or map an old name with <code>@renamed_from</code>.
        </p>
      )}
      {(versions.older > 0 || versions.unknown > 0) && (
        <p className="settings-help">
          Facts in the tenant scope: {versions.older} written under an older vocabulary than the
          current one (version {state.ontology_version}), {versions.unknown} with no recorded
          version.
        </p>
      )}
      {(state.undated_statements ?? 0) > 0 && (
        <p className="settings-help">
          {state.undated_statements} statement(s) of a dated relation kind carry no date.
        </p>
      )}
    </>
  );
}

// OntologySection is the tenant ontology's page.
//
// The failure it exists to prevent is not an error message: it is an operator who edits
// the ontology document, never learns that editing alone changes nothing, and concludes
// the feature is broken. So the page leads with the state of the gate, then what needs a
// decision (proposals), then the vocabulary one section per tab, each showing what is in
// force rather than only what is written.
//
// Authoring stays in the document editor (a Markdown document deserves it); the gate,
// the layered result and the review of proposals cannot live there, so they are here.
export default function OntologySection() {
  // The topbar user picker IS the survey scope: facts live per user, so which user the
  // curation run belongs to decides what it can see.
  const pickedUser = useUserId();
  const [params] = useSearchParams();
  const [state, setState] = useState<OntologyResponse | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    try {
      setState(await getOntology());
      setErr(null);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  // Every action re-reads the whole state from its response, so the page shows the
  // effective result rather than an optimistic guess.
  const act = async (call: () => Promise<OntologyResponse>) => {
    if (busy) return;
    setBusy(true);
    try {
      setState(await call());
      setErr(null);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  // RFC CA: accept puts the entity in force where it already sits; reject keeps it as a
  // tombstone so a curator stops re-proposing it.
  const resolveProposal = (chunkId: string, action: "accept" | "reject") =>
    act(() => resolveOntologyProposal(chunkId, action));
  const adopt = (name: string, section?: "relations") =>
    act(() => adoptOntologyType(name, undefined, section));
  const flip = (status: "confirmed" | "draft") => act(() => setOntologyStatus(status));

  // Split by status rather than filtering twice at the render site: pending needs a
  // decision, rejected is a record.
  const allProposals: OntologyProposal[] = state?.proposals ?? [];
  const pendingProposals = allProposals.filter((p) => p.status === "proposed");
  const rejectedProposals = allProposals.filter((p) => p.status === "rejected");
  // A status that is neither of the two words the gate accepts — reachable by editing
  // the document's status field by hand. Called out explicitly, because this is
  // precisely the state that looks confirmed and behaves as draft.
  const oddStatus = !!state?.status && state.status !== "confirmed" && state.status !== "draft";
  const tabs = state?.provisioned ? ontologyTabs(state) : [];
  const tab = pickOntologyTab(params.get("section"), tabs);
  const path = state?.path ?? "/memory/ontology";

  return (
    <div className="settings-panel">
      <h2>Ontology</h2>
      <p className="settings-help">
        The tenant&rsquo;s shared vocabulary: the classes agents extract and documents are typed
        by, the relation kinds that link them, the concept schemes that classify documents, the
        real organisations and posts the tenant knows, and the grammars that read citations.
        Every tenant starts from a standard set; yours is layered on top once you confirm it.
        Until then your edits are stored and inert.
      </p>

      {/* THE EDIT AFFORDANCE: a labelled action that lands ON the document, not a path in
          prose. ?scope=tenant is load-bearing — the viewer folds an absent scope to
          `user`, which opened the right id in the wrong store (ontologyEditHref). Falls
          back to the Paths browser only when there is no id to address. */}
      <p className="settings-help">
        {state?.document_id ? (
          <Link className="settings-action-link" to={ontologyEditHref(state.document_id)}>
            Edit ontology →
          </Link>
        ) : (
          <Link to="/documents">{path}</Link>
        )}{" "}
        <span className="settings-muted-inline">{path} (tenant scope)</span>
      </p>

      {err && <div className="settings-error">{err}</div>}

      {state && !state.provisioned && (
        <div className="settings-muted">
          No ontology document — this deployment has no SQL Memory configured, so agents run on
          the standard types alone.
        </div>
      )}

      {state?.provisioned && (
        <>
          <div className="settings-row">
            <span className={state.confirmed ? "settings-flash" : "settings-muted"}>
              {state.confirmed ? "Confirmed — in force" : "Draft — NOT in force"}
              {state.ontology_version ? ` · version ${state.ontology_version}` : ""}
              {(state.sections ?? []).length > 0
                ? ` · sections: ${(state.sections ?? []).join(", ")}`
                : " · classes only (no @section)"}
            </span>
            {state.confirmed ? (
              <button type="button" onClick={() => void flip("draft")} disabled={busy}>
                Revert to draft
              </button>
            ) : (
              <button type="button" onClick={() => void flip("confirmed")} disabled={busy}>
                Confirm
              </button>
            )}
          </div>

          {(state.notes ?? []).map((n) => (
            <p className="settings-help" key={n}>
              {n}
            </p>
          ))}

          {oddStatus && (
            <p className="settings-help">
              The document&rsquo;s status reads <code>{state.status}</code>, which is neither{" "}
              <code>draft</code> nor <code>confirmed</code> — the tenant layer is treated as
              draft. Use the button above rather than editing the status by hand; only the exact
              word activates it.
            </p>
          )}

          {/* PROPOSALS — above the tabs on purpose: they are the only thing here that
              needs a decision, whichever section they belong to, and a pending decision
              buried in a tab is a decision nobody makes. */}
          {pendingProposals.length > 0 && (
            <div className="settings-row ontology-proposals">
              <div>
                <strong>
                  {pendingProposals.length} suggestion{pendingProposals.length === 1 ? "" : "s"}
                </strong>{" "}
                <span className="settings-muted">
                  not in force until you accept — accepting keeps each one exactly where it sits
                  in the document
                </span>
                {pendingProposals.map((p) => (
                  <div className="ontology-proposal" key={p.chunk_id}>
                    <div>
                      <code>{p.name}</code>
                      <span className="settings-muted"> {proposalKind(p)}</span>
                      {p.parent && (
                        <span className="settings-muted">
                          {" "}
                          under <code>{p.parent}</code>
                        </span>
                      )}
                      {p.fields && p.fields.length > 0 && (
                        <span className="settings-muted"> — {p.fields.join(", ")}</span>
                      )}
                    </div>
                    {/* The evidence, so the operator judges a case and not a name. */}
                    {p.body && <pre className="ontology-evidence">{p.body.trim()}</pre>}
                    <div className="settings-row-actions">
                      <button
                        type="button"
                        disabled={busy}
                        onClick={() => void resolveProposal(p.chunk_id, "accept")}
                      >
                        accept
                      </button>
                      <button
                        type="button"
                        className="ghost-btn"
                        disabled={busy}
                        onClick={() => void resolveProposal(p.chunk_id, "reject")}
                      >
                        reject
                      </button>
                    </div>
                  </div>
                ))}
              </div>
            </div>
          )}

          {/* Where suggestions COME FROM. A link, not a button — it stages the run in the
              terminal so the agent and prompt are visible before tokens are spent. */}
          <p className="settings-help">
            <Link className="settings-action-link" to={ontologistRunHref(pickedUser)}>
              Review suggestions →
            </Link>{" "}
            <span className="settings-muted">
              stages a pass that reads {pickedUser ? <code>{pickedUser}</code> : "a user"}
              &rsquo;s stored facts and files suggestions here. It cannot change anything on its
              own — everything it files waits for you.
            </span>
          </p>

          {rejectedProposals.length > 0 && (
            <details className="settings-help">
              <summary>
                {rejectedProposals.length} rejected suggestion
                {rejectedProposals.length === 1 ? "" : "s"}
              </summary>
              <p>
                Kept on purpose: a rejection is a record, so an automated curator can see what you
                already turned down instead of proposing it again. They are not in force, and
                never will be unless you clear the status in the document editor.
              </p>
              {rejectedProposals.map((p) => (
                <div key={p.chunk_id}>
                  <code>{p.name}</code>
                  <span className="settings-muted"> {proposalKind(p)}</span>
                  {p.parent && (
                    <span className="settings-muted">
                      {" "}
                      under <code>{p.parent}</code>
                    </span>
                  )}
                </div>
              ))}
            </details>
          )}

          {/* The sections, one per tab. The tab is in the URL (?section=) so a link
              to "the ontology's relations" survives a reload and can be shared. */}
          <div className="library-subtabs ontology-tabs" role="tablist">
            {tabs.map((t) => (
              <Link
                key={t.id}
                to={`?section=${t.id}`}
                role="tab"
                aria-selected={t.id === tab}
                className={"library-subtab" + (t.id === tab ? " library-subtab-active" : "")}
              >
                {t.label} <span className="library-subtab-count">{t.count}</span>
                {t.warn && <span className="ontology-warn"> !</span>}
              </Link>
            ))}
          </div>

          <div className="ontology-tab-body">
            {tab === "classes" && (
              <OntologyClassesTab state={state} busy={busy} onAdopt={(name) => void adopt(name)} />
            )}
            {tab === "document-classes" && <OntologyDocumentClassesTab state={state} />}
            {tab === "relations" && (
              <OntologyRelationsDetail
                state={state}
                busy={busy}
                onAdopt={(name) => void adopt(name, "relations")}
              />
            )}
            {tab === "schemes" && <OntologySchemesDetail state={state} />}
            {tab === "instances" && <OntologyInstancesDetail state={state} />}
            {tab === "grammars" && <OntologyGrammarsTab state={state} />}
            {tab === "health" && <OntologyHealthTab state={state} />}
          </div>

          <OntologyFormatHelp />
        </>
      )}
    </div>
  );
}

// proposalKind names what a suggestion would become, so a relation kind is not judged
// as if it were an entity type.
function proposalKind(p: OntologyProposal): string {
  switch (p.section) {
    case "relations":
      return "relation kind";
    case "schemes":
      return "concept";
    case "instances":
      return "instance";
    default:
      return "class";
  }
}

// OntologyFormatHelp states the document's format rather than leaving it to be inferred
// from a template — guessing it was half of the report that the ontology was unusable.
function OntologyFormatHelp() {
  return (
    <details className="settings-help">
      <summary>How the document is written</summary>
      <p>
        <strong>Sections.</strong> Each top-level heading may open a section by putting{" "}
        <code>@section classes</code>, <code>relations</code>, <code>schemes</code> or{" "}
        <code>instances</code> on its first line. A document with no <code>@section</code> is
        all classes.
      </p>
      <p>
        <strong>Classes.</strong> A heading names a class and each <code>- `field`</code>{" "}
        bullet declares a field; prose around them is documentation. Nest a heading to make a
        subclass: it inherits every field above it, and a search for the general class also
        finds the specific ones. Reuse a standard type&rsquo;s name to override it.{" "}
        A class under <code>Document</code> types documents rather than facts, with{" "}
        <code>@title_pattern</code>, <code>@identifier_pattern</code>,{" "}
        <code>@citation_grammar</code> and <code>@anchor_grammar</code>.
      </p>
      <p>
        <strong>Grammars</strong> sit in the classes section under a heading{" "}
        <code>grammar:&lt;name&gt;</code>, one rule per line: <code>pattern:</code> and{" "}
        <code>resolve:</code> for citations, <code>scope: &lt;pattern&gt; =&gt; &lt;alt&gt; |
        &lt;alt&gt;</code> and <code>anchor: …</code> to key a document&rsquo;s parts.
      </p>
      <p>
        <strong>Relations</strong> nest as sub-kinds (<em>amends</em> under <em>changes</em>);{" "}
        <strong>schemes</strong> nest concepts broader to narrower, without inheritance;{" "}
        <strong>instances</strong> are the real organisations, units and posts, each naming its
        class with <code>@a &lt;class&gt;</code>, adopted into memory on confirm.
      </p>
      <p>
        Changes take effect on the next run once the document is confirmed. Revise the document
        in place: a replacement document gives every concept a new id.
      </p>
    </details>
  );
}

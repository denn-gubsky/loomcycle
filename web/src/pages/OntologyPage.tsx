import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useUserId } from "../components/Layout";
import { ontologistRunHref, ontologyEditHref } from "../lib/ontologyEditHref";
import {
  buildRelationTree,
  classDirectiveNotes,
  conceptSummary,
  instanceStateLabel,
  factsUnderOlderVocabulary,
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
// layered result, section by section. Moved out of SectionPages unchanged.

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

// OntologySectionsDetail is the part of the panel a SECTIONED ontology adds (RFC FO):
// the document classes — kept out of the table above because they never reach an
// agent's prompt — the grammars, the types in use that nothing declares, and how many
// facts were written under an older vocabulary. Relations, schemes and instances each
// have their own block below.
function OntologySectionsDetail({
  state,
  busy,
  onAdoptRelation,
}: {
  state: OntologyResponse;
  busy: boolean;
  onAdoptRelation: (name: string) => void;
}) {
  const versions = factsUnderOlderVocabulary(state);
  return (
    <>
      <p className="settings-help">
        Sections: {(state.sections ?? []).map((k) => <code key={k}>{k} </code>)}
      </p>
      {(state.document_classes ?? []).length > 0 && (
        <div className="settings-row">
          <div>
            <strong>Document classes</strong>
            <span className="settings-muted">
              {" "}
              — govern a document's type; never listed to agents as entity types.
            </span>
            <OntologyTermTree nodes={buildOntologyTree(state.document_classes ?? [])} markSource />
          </div>
        </div>
      )}
      {(state.grammars ?? []).length > 0 && (
        <div className="settings-row">
          <div>
            <strong>Grammars</strong>
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
                </li>
              ))}
            </ul>
          </div>
        </div>
      )}
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
      {state.relations !== undefined && (
        <OntologyRelationsDetail state={state} busy={busy} onAdopt={onAdoptRelation} />
      )}
      {state.schemes !== undefined && <OntologySchemesDetail state={state} />}
      {state.instances !== undefined && <OntologyInstancesDetail state={state} />}
      {(versions.older > 0 || versions.unknown > 0) && (
        <p className="settings-help">
          Facts in the tenant scope: {versions.older} written under an older vocabulary
          than the current one (version {state.ontology_version}), {versions.unknown} with no
          recorded version.
        </p>
      )}
    </>
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

// OntologySection is the operator control for the tenant's entity types.
//
// The failure this exists to prevent is not an error message: it is an operator
// who edits the ontology document, never learns that editing alone changes
// nothing, and concludes the feature is broken. So the panel leads with the state
// of the gate, and shows the tenant's own types NEXT TO the types actually in
// force — the gap between the two columns is the only visible evidence that a
// draft is inert.
//
// Authoring stays in the Path browser (a Markdown document deserves the document
// editor); the two things that cannot live there are the gate and the layered
// result, so those are what this panel is.
export default function OntologySection() {
  // The topbar user picker IS the survey scope: facts live per user, so which user the
  // curation run belongs to decides what it can see.
  const pickedUser = useUserId();
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

  // RFC CA: accept puts the entity in force where it already sits; reject keeps it as a
  // tombstone so a curator stops re-proposing it. Both re-read the whole state from the
  // response, so the panel shows the effective result rather than an optimistic guess.
  const resolveProposal = async (chunkId: string, action: "accept" | "reject") => {
    if (busy) return;
    setBusy(true);
    try {
      setState(await resolveOntologyProposal(chunkId, action));
      setErr(null);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const adopt = async (name: string, section?: "relations") => {
    if (busy) return;
    setBusy(true);
    try {
      setState(await adoptOntologyType(name, undefined, section));
      setErr(null);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const flip = async (status: "confirmed" | "draft") => {
    if (busy) return;
    setBusy(true);
    try {
      setState(await setOntologyStatus(status));
      setErr(null);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const tenantTerms = state?.terms ?? [];
  // Split by status rather than filtering twice at the render site: pending needs a
  // decision, rejected is a record. Conflating them would put "reject" buttons next to
  // things already rejected.
  const allProposals: OntologyProposal[] = state?.proposals ?? [];
  const pendingProposals = allProposals.filter((p) => p.status === "proposed");
  const rejectedProposals = allProposals.filter((p) => p.status === "rejected");
  // A status that is neither of the two words the gate accepts — reachable by
  // editing the document's status field by hand. Called out explicitly, because
  // this is precisely the state that looks confirmed and behaves as draft.
  const oddStatus =
    !!state?.status && state.status !== "confirmed" && state.status !== "draft";

  return (
    <div className="settings-panel">
      <h2>Ontology</h2>
      <p className="settings-help">
        The entity types agents extract against. Every tenant starts from a
        standard set; the types you define are layered on top — but only once
        you confirm them below. Until then your edits are stored and inert, so
        you can draft an ontology without changing what any running agent is
        told.
      </p>

      {/* THE EDIT AFFORDANCE, and it is deliberately a button rather than the
          path in prose. An operator reported being unable to find any way to
          change the ontology: this panel renders types and offers "Revert to
          draft", so it reads as the place you edit them, while the only route in
          was a sentence linking to the Paths browser — a file tree, from which
          you still had to know which document to open. A labelled action that
          lands ON the document removes the guess.

          Links by document_id when the API returned one (it is provisioned and
          the caller may read it); falls back to the Paths browser only when
          there is no id to address, which is the un-provisioned case the panel
          already explains below.

          ?scope=tenant IS LOAD-BEARING. The ontology lives at tenant scope, and
          the viewer folds an absent scope to `user` — so without it this link
          opened the right document id in the wrong store, the read 422'd, and the
          operator got "No chunks" and no create buttons. The affordance existed
          and led nowhere, which is most of why the ontology UI was reported
          unusable. */}
      <p className="settings-help">
        {state?.document_id ? (
          <Link className="settings-action-link" to={ontologyEditHref(state.document_id)}>
            Edit ontology →
          </Link>
        ) : (
          <Link to="/documents">{state?.path ?? "/memory/ontology"}</Link>
        )}
        {" "}
        <span className="settings-muted-inline">
          {state?.path ?? "/memory/ontology"} (tenant scope)
        </span>
      </p>

      {/* The format, stated here rather than left to be inferred from the
          template. Guessing it was the second half of the same report. */}
      <details className="settings-help">
        <summary>How the document is written</summary>
        <p>
          Each section is one entity type: the <code>## heading</code> names it,
          and each <code>- `field`</code> bullet declares a field. Prose around
          them is documentation and is ignored. Reuse a standard type's name to
          override its fields.
        </p>
        <p>
          <strong>Nest a heading to make a subclass.</strong> A{" "}
          <code>### incident</code> under <code>## event</code> is a kind of event:
          it inherits every field above it and adds its own, and a search for the
          general type also finds the specific ones. Nest up to four levels. To
          subclass a <em>standard</em> type, give it a <code>##</code> heading of
          its own first — that overrides the standard one — then nest beneath your
          copy. <code>preference</code> and <code>fact</code> are the memory tier's
          own types and always stay top-level: you may nest types <em>under</em> them,
          but not them under something else.
        </p>
        <p>
          <strong>In the editor:</strong> select the type you want to extend, then
          use <strong>+ child</strong> to nest a subtype under it (<strong>+ text</strong>{" "}
          adds a sibling at the same level instead). Rename it in the dialog that
          opens — a type's name is its heading — and list its own fields as
          backticked bullets in the body. Changes take effect on the next run once
          the document is confirmed.
        </p>
      </details>

      {err && <div className="settings-error">{err}</div>}

      {state && !state.provisioned && (
        <div className="settings-muted">
          No ontology document — this deployment has no SQL Memory configured, so
          agents run on the standard types alone.
        </div>
      )}

      {state?.provisioned && (
        <>
          <div className="settings-row">
            <span className={state.confirmed ? "settings-flash" : "settings-muted"}>
              {state.confirmed
                ? `Confirmed — your ${tenantTerms.length} type(s) are in force.`
                : `Draft — your ${tenantTerms.length} type(s) are NOT in force.`}
            </span>
            {state.confirmed ? (
              <button type="button" onClick={() => flip("draft")} disabled={busy}>
                Revert to draft
              </button>
            ) : (
              <button
                type="button"
                onClick={() => flip("confirmed")}
                disabled={busy}
              >
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
              The document's status reads <code>{state.status}</code>, which is
              neither <code>draft</code> nor <code>confirmed</code> — the tenant
              layer is treated as draft. Use the button above rather than editing
              the status by hand; only the exact word activates it.
            </p>
          )}

          {state.confirmed && tenantTerms.length === 0 && (
            <p className="settings-help">
              Confirmed, but no types were found in the document. Each type needs
              its own <code>## name</code> heading, with field names in backticks
              on the bullets beneath it — anything else is skipped, so a
              differently-formatted document confirms to nothing. Nest a heading
              (<code>### name</code>) to make that type a <em>subclass</em> of the
              one above it.
            </p>
          )}

          {/* PROPOSALS — entities in the document that are not in force. Above the
              table on purpose: they are the only thing here that needs a decision,
              and a pending decision buried under a type listing is a decision nobody
              makes. Rejected ones are kept as tombstones (so a curator stops
              re-proposing them) but folded away, since they compete with live types
              for attention and never need action. */}
          {pendingProposals.length > 0 && (
            <div className="settings-row ontology-proposals">
              <div>
                <strong>
                  {pendingProposals.length} suggested type
                  {pendingProposals.length === 1 ? "" : "s"}
                </strong>{" "}
                <span className="settings-muted">
                  not in force until you accept — accepting keeps each one exactly where
                  it sits in the document
                </span>
                {pendingProposals.map((p) => (
                  <div className="ontology-proposal" key={p.chunk_id}>
                    <div>
                      <code>{p.name}</code>
                      {p.section === "relations" && (
                        <span className="settings-muted"> relation kind</span>
                      )}
                      {p.section === "schemes" && <span className="settings-muted"> concept</span>}
                      {p.section === "instances" && <span className="settings-muted"> instance</span>}
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

          {/* Where suggestions COME FROM. Placed with them rather than at the top of the
              panel: an operator reading a list of suggestions is the one who wants
              another pass, and an operator who has never seen one needs to know a pass
              is a thing they can ask for. A link, not a button — it stages the run in
              the terminal so the agent and prompt are visible before tokens are spent. */}
          <p className="settings-help">
            <Link className="settings-action-link" to={ontologistRunHref(pickedUser)}>
              Review suggestions →
            </Link>{" "}
            <span className="settings-muted">
              stages a pass that reads {pickedUser ? <code>{pickedUser}</code> : "a user"}
              &rsquo;s stored facts and files suggestions here. It cannot change anything
              on its own — everything it files waits for you.
            </span>
          </p>

          {rejectedProposals.length > 0 && (
            <details className="settings-help">
              <summary>
                {rejectedProposals.length} rejected type
                {rejectedProposals.length === 1 ? "" : "s"}
              </summary>
              <p>
                Kept on purpose: a rejection is a record, so an automated curator can
                see what you already turned down instead of proposing it again. They are
                not in force, and they never will be unless you clear the status in the
                document editor.
              </p>
              {rejectedProposals.map((p) => (
                <div key={p.chunk_id}>
                  <code>{p.name}</code>
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

          {/* ADOPT — the standard types this document does not declare. Offered here
              because the documented way to subclass one is to declare it yourself
              first, which otherwise means retyping its field names off the column on
              the right, by hand, correctly. */}
          {(state.adoptable ?? []).length > 0 && (
            <p className="settings-help">
              <strong>Extend a standard type:</strong> adopting one copies it into your
              document with its fields, so you can add to it or nest subtypes under it.
              Your copy then overrides the standard one, wholesale — a field a later
              loomcycle release adds to it will not reach your copy.
              <span className="ontology-adopt-row">
                {(state.adoptable ?? []).map((name) => (
                  <button
                    key={name}
                    type="button"
                    className="ghost-btn"
                    disabled={busy}
                    onClick={() => void adopt(name)}
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
                    <OntologyTermTree
                      nodes={buildOntologyTree(state.effective ?? [])}
                      markSource
                    />
                  </td>
                </tr>
              </tbody>
            </table>
          </div>

          {(state.sections ?? []).length > 0 && (
            <OntologySectionsDetail
              state={state}
              busy={busy}
              onAdoptRelation={(name) => void adopt(name, "relations")}
            />
          )}
        </>
      )}
    </div>
  );
}

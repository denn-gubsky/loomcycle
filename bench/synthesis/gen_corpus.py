#!/usr/bin/env python3
"""RFC DB-2 — generate the cross-session synthesis corpus.

Deterministic on purpose. RFC DB section 7 pins extractor sampling so that
EXTRACTION is the only stochastic part of the benchmark; a corpus written by an
LLM would put noise back underneath it, where no seed can reach. Everything here
comes from a seeded RNG over templates.

The corpus is a typed relational graph. Each edge becomes exactly one fact, each
fact lands in exactly one session, and a question is a PATH through the graph --
so the hop count is known by construction rather than judged after the fact.

Three properties are enforced, not hoped for (see validate()):
  1. no two supports of a question share a session   -> it is cross-session
  2. no fact mentioning the anchor also mentions the gold -> ONE HOP CANNOT REACH IT
  3. the answer is unique among entities of its kind -> the gold is gradeable

Property 2 is the one DB-2 exists for. A question one hop can answer measures
retrieval volume, not traversal.
"""
import datetime, json, os, random, sys, itertools
from collections import defaultdict

SEED = int(os.environ.get("DB2_SEED", "20260915"))
N_QUESTIONS = int(os.environ.get("DB2_QUESTIONS", "120"))
MAX_FACTS_PER_SESSION = 6

rng = random.Random(SEED)

# ---------------------------------------------------------------- names
# Invented throughout. The no-memory arm measures parametric leakage, so a name
# that collides with a real person or company would turn that control into a
# world-knowledge test -- which is how CO-2 scored 14/20 from training alone.
ON1 = ["Vel", "Thorn", "Kes", "Hall", "Brack", "Mor", "Card", "Ely", "Fen", "Gal",
       "Hask", "Ind", "Jar", "Kel", "Lum", "Nard", "Oss", "Pel", "Quill", "Rast",
       "Sarn", "Tarn", "Umb", "Vask", "Wren", "Yarr", "Zeb", "Alk", "Bram", "Cirr"]
ON2 = ["mara", "beck", "trel", "oway", "ridge", "vane", "well", "thorpe", "mont", "dale",
       "stead", "haven", "forge", "cross", "gate", "field", "hollow", "reach", "spire", "moor"]
ON3 = ["Instruments", "Labs", "Freight", "Mills", "Dynamics", "Systems", "Foundry",
       "Holdings", "Works", "Analytics", "Chemical", "Optics", "Logistics", "Fabrication",
       "Textiles", "Ceramics", "Avionics", "Hydraulics", "Refining", "Cartage"]

PN1 = ["Ines", "Odo", "Petra", "Marek", "Suri", "Dalia", "Osric", "Tamsin", "Bram", "Nerea",
       "Calder", "Vespa", "Loam", "Rhys", "Imke", "Torvald", "Anouk", "Bes", "Corin", "Delphine",
       "Emrys", "Fenna", "Gero", "Hilde", "Ivo", "Joska", "Kesia", "Lars", "Mira", "Nils",
       "Orla", "Pavo", "Quint", "Rune", "Sable", "Toma", "Ulla", "Vidar", "Wilna", "Yara"]
PN2 = ["Farrow", "Brannigan", "Voss", "Dulaney", "Anand", "Kestrelby", "Marchetti", "Oyelaran",
       "Sandoval", "Threlkeld", "Ubberud", "Vantongeren", "Wexley", "Yannopoulos", "Zabala",
       "Ashgrove", "Beaumarchais", "Coldwater", "Dunmore", "Everleigh", "Fairweather",
       "Grimsby", "Holloway", "Ivarsson", "Jorgensen", "Kettleburn", "Lindqvist", "Merriweather",
       "Nordstrom", "Ottoline", "Pemberton", "Quintrell", "Rothery", "Stavropoulos", "Trevelyan",
       "Underwood", "Vasquez", "Whitlock", "Yardley", "Zoltan"]

CN1 = ["Ald", "Pell", "Corv", "Brin", "Dun", "Esk", "Fal", "Grim", "Hoth", "Ist",
       "Jen", "Kirk", "Lang", "Mel", "Nor", "Orm", "Pry", "Quen", "Rud", "Sel",
       "Tor", "Uls", "Vad", "Wyn", "Yel", "Zar", "Aber", "Bel", "Cran", "Dro",
       "Ferr", "Gled", "Harn", "Ith", "Kald", "Lorn", "Mard", "Neth", "Ost", "Pens",
       "Ravn", "Sten", "Thal", "Vern", "Wold"]
CN2 = ["bury", "moor", "ane", "kirk", "holm", "wich", "ton", "stad", "ford", "mere",
       "heath", "combe", "worth", "by", "shaw"]

RN = ["Ostrel", "Marren", "Calvane", "Drenholt", "Everweald", "Fenmark", "Gallowmere",
      "Hesper", "Ilmarch", "Jorvane", "Kelmoor", "Lystrand", "Nordhal", "Oakmere", "Pyrran",
      "Quarrenmoor", "Rhosgar", "Silvane", "Tarnwick", "Ulverbeck", "Vantholt", "Weyrmarch",
      "Xanthe", "Yelverton", "Zorrand", "Ashfen", "Brackmoor", "Cindervale", "Dunhallow",
      "Elmsworth"]

JN1 = ["Larkspur", "Wren", "Ferrule", "Camber", "Dovetail", "Escarp", "Fathom", "Gantry",
       "Harrow", "Inlet", "Jetty", "Keelson", "Lintel", "Mullion", "Newel", "Oriel",
       "Purlin", "Quoin", "Rafter", "Soffit", "Transom", "Undercroft", "Voussoir",
       "Wainscot", "Yoke", "Zenith", "Abutment", "Bollard", "Corbel", "Dado",
       "Entasis", "Finial", "Groin", "Haunch", "Impost", "Joist", "Keystone", "Lierne",
       "Modillion", "Nogging"]


def uniq(gen, n, label):
    out, seen = [], set()
    guard = 0
    while len(out) < n:
        guard += 1
        if guard > n * 200:
            sys.exit("could not generate %d unique %s" % (n, label))
        v = gen()
        if v not in seen:
            seen.add(v)
            out.append(v)
    return out


PEOPLE = uniq(lambda: "%s %s" % (rng.choice(PN1), rng.choice(PN2)), 80, "people")
ORGS = uniq(lambda: "%s%s %s" % (rng.choice(ON1), rng.choice(ON2), rng.choice(ON3)), 30, "orgs")
CITIES = uniq(lambda: "%s%s" % (rng.choice(CN1), rng.choice(CN2)), 45, "cities")
REGIONS = ["the %s region" % r for r in RN]
PROJECTS = ["Project %s" % j for j in uniq(lambda: rng.choice(JN1), 40, "projects")]

# ---------------------------------------------------------------- the graph
# Every relation is a FUNCTION (one object per subject), which is what makes a
# path's answer unique and therefore gradeable.
rel = {}            # (subject, relation) -> object
facts = []          # [{id, text, s, r, o}]


def edge(s, r, o, text):
    if (s, r) in rel:
        return
    rel[(s, r)] = o
    facts.append({"id": "f%d" % (len(facts) + 1), "s": s, "r": r, "o": o, "text": text})


for c in CITIES:
    r = rng.choice(REGIONS)
    edge(c, "in_region", r, "%s is in %s." % (c, r))
for o in ORGS:
    c = rng.choice(CITIES)
    edge(o, "based_in", c, "%s is based in %s." % (o, c))
for p in PEOPLE:
    o = rng.choice(ORGS)
    edge(p, "works_at", o, "%s works at %s." % (p, o))
for p in PEOPLE:
    c = rng.choice(CITIES)
    edge(p, "lives_in", c, "%s lives in %s." % (p, c))
for i, j in enumerate(PROJECTS):
    o = rng.choice(ORGS)
    edge(j, "owned_by", o, "%s is owned by %s." % (j, o))
# one manager per project, and a person manages at most one project
managers = rng.sample(PEOPLE, len(PROJECTS))
for j, p in zip(PROJECTS, managers):
    edge(p, "manages", j, "%s manages %s." % (p, j))
# a reporting line inside the same org, so the chain stays coherent
by_org = defaultdict(list)
for p in PEOPLE:
    by_org[rel[(p, "works_at")]].append(p)
for o, members in by_org.items():
    if len(members) < 2:
        continue
    head = members[0]
    for p in members[1:]:
        edge(p, "reports_to", head, "%s reports to %s." % (p, head))
# acquisitions: a minority of orgs, never cyclic (low index acquires high)
acq = rng.sample(range(len(ORGS)), max(2, len(ORGS) // 4))
for idx in acq:
    if idx == 0:
        continue
    buyer = ORGS[rng.randrange(0, idx)]
    edge(ORGS[idx], "acquired_by", buyer, "%s was acquired by %s." % (ORGS[idx], buyer))

FACT_BY_EDGE = {(f["s"], f["r"]): f for f in facts}

# ---------------------------------------------------------------- questions
# Each template is a path. hops == number of facts the chain needs.
TEMPLATES = [
    (["works_at", "based_in"], "Which city does %s work in?", PEOPLE),
    (["based_in", "in_region"], "Which region is %s in?", ORGS),
    (["manages", "owned_by"], "Which organisation owns the project %s manages?", PEOPLE),
    (["works_at", "acquired_by"], "Which company ultimately employs %s now?", PEOPLE),
    (["works_at", "based_in", "in_region"], "Which region does %s work in?", PEOPLE),
    (["manages", "owned_by", "based_in"], "In which city is the owner of the project %s manages based?", PEOPLE),
    (["reports_to", "works_at", "based_in"], "In which city is the employer of %s's manager based?", PEOPLE),
    (["manages", "owned_by", "based_in", "in_region"], "Which region is the owner of the project %s manages in?", PEOPLE),
    (["reports_to", "manages", "owned_by"], "Which organisation owns the project managed by the manager of %s?", PEOPLE),
    (["works_at", "acquired_by", "based_in"], "In which city is the ultimate employer of %s based?", PEOPLE),
]


def walk(start, rels):
    """Resolve a path; return (supports, gold) or None."""
    cur, sup = start, []
    for r in rels:
        f = FACT_BY_EDGE.get((cur, r))
        if f is None:
            return None
        sup.append(f["id"])
        cur = f["o"]
    if len(set(sup)) != len(sup):
        return None
    return sup, cur


candidates = []
for rels, qt, starts in TEMPLATES:
    for s in starts:
        got = walk(s, rels)
        if got is None:
            continue
        sup, gold = got
        candidates.append({"anchor": s, "rels": rels, "hops": len(rels),
                           "q": qt % s, "gold": gold, "supports": sup})
rng.shuffle(candidates)

# ---------------------------------------------------------------- sessions
# Facts whose co-appearance would make a question single-session must not share
# one. Colour the conflict graph greedily under a capacity bound; everything
# unconstrained fills in afterwards as distractor content.
conflict = defaultdict(set)
for c in candidates:
    for a, b in itertools.combinations(c["supports"], 2):
        conflict[a].add(b)
        conflict[b].add(a)

order = sorted((f["id"] for f in facts), key=lambda fid: -len(conflict[fid]))
sessions = []            # list of lists of fact ids
session_of = {}
for fid in order:
    placed = False
    for si, members in enumerate(sessions):
        if len(members) >= MAX_FACTS_PER_SESSION:
            continue
        if conflict[fid] & set(members):
            continue
        members.append(fid)
        session_of[fid] = si
        placed = True
        break
    if not placed:
        sessions.append([fid])
        session_of[fid] = len(sessions) - 1

FACT = {f["id"]: f for f in facts}
for f in facts:
    f["session"] = "s%d" % (session_of[f["id"]] + 1)

# ---------------------------------------------------------------- selection
# Property 2, the reason DB-2 exists: drop any question a single fact could
# answer, i.e. where some fact naming the anchor also names the gold.
by_entity = defaultdict(list)
for f in facts:
    by_entity[f["s"]].append(f)
    by_entity[f["o"]].append(f)


def one_hop_reaches(anchor, gold):
    for f in by_entity[anchor]:
        if gold in f["text"] and anchor in f["text"]:
            return True
    return False


kept = []
dropped_one_hop = 0
for c in candidates:
    if one_hop_reaches(c["anchor"], c["gold"]):
        dropped_one_hop += 1
        continue
    ses = {FACT[s]["session"] for s in c["supports"]}
    if len(ses) != len(c["supports"]):
        continue                        # two supports landed in one session
    kept.append(c)

# hold the hop mix steady so DB-3 can report per-hop accuracy on real numbers
want = {2: int(N_QUESTIONS * 0.40), 3: int(N_QUESTIONS * 0.40)}
want[4] = N_QUESTIONS - want[2] - want[3]
chosen, used_anchor = [], defaultdict(int)
for h in (2, 3, 4):
    pool = [c for c in kept if c["hops"] == h]
    for c in pool:
        if len(chosen) >= sum(want.values()):
            break
        if len([x for x in chosen if x["hops"] == h]) >= want[h]:
            break
        if used_anchor[(c["anchor"], h)]:
            continue                    # one question per (anchor, depth)
        used_anchor[(c["anchor"], h)] = 1
        chosen.append(c)

for i, c in enumerate(chosen, 1):
    c["id"] = "q%d" % i

POOL_OF = {"city": CITIES, "region": REGIONS, "org": ORGS}


def gold_pool(gold):
    for name, pool in POOL_OF.items():
        if gold in pool:
            return name, len(pool)
    return "?", 0


# ---------------------------------------------------------------- sessions as chat
OPEN = ["Quick note from today.", "Catching you up.", "One thing worth recording.",
        "Some housekeeping.", "Notes from the call.", "Update for the record.",
        "Passing this along.", "For the file."]
ACK = ["Noted.", "Got it, recorded.", "Understood.", "Logged.", "Thanks, saved.",
       "Recorded.", "Okay, noted.", "Filed."]


# Sessions are dated one per day, oldest first. The date is not decoration: the
# extractor reads it to stamp observed_at, and Turn.Body() prefixes it into the
# embedded text, so a corpus without dates ingests differently from a real one.
START_DATE = datetime.date(2026, 1, 5)


def render(sid, session_facts):
    """One fact per user turn, each acknowledged.

    A session is what the consolidation path actually reads, so it is shaped like
    a conversation rather than a wall of text. NOTE for DB-3: this yields about
    2x(facts+1) turns per session, which stays BELOW the extractor's windowing
    threshold -- so these runs exercise whole-session extraction, not windowed.
    That is a property to report, not to silently inherit.

    Each fact-bearing turn gets a stable dia_id so the retrieval answer key can
    name the exact turn rather than the session.
    """
    turns = [{"role": "user", "dia_id": "%s:t0" % sid, "text": rng.choice(OPEN)},
             {"role": "assistant", "dia_id": "%s:t1" % sid, "text": rng.choice(ACK)}]
    for f in session_facts:
        f["turn_dia_id"] = "%s:t%d" % (sid, len(turns))
        turns.append({"role": "user", "dia_id": f["turn_dia_id"], "text": f["text"]})
        turns.append({"role": "assistant", "dia_id": "%s:t%d" % (sid, len(turns)),
                      "text": rng.choice(ACK)})
    return turns


session_rows = []
for si, members in enumerate(sessions):
    sid = "s%d" % (si + 1)
    sf = [FACT[m] for m in members]
    session_rows.append({"id": sid,
                         "date": (START_DATE + datetime.timedelta(days=si)).isoformat(),
                         "fact_ids": members,
                         "turns": render(sid, sf)})

# ---------------------------------------------------------------- validate
def validate():
    errs = []
    # one_hop_reaches matches entity names as SUBSTRINGS. If one name contains
    # another, that test quietly answers about the wrong entity and property 2
    # stops being enforced -- so the names have to be checked, not assumed.
    names = PEOPLE + ORGS + CITIES + REGIONS + PROJECTS
    for a, b in itertools.permutations(names, 2):
        if a in b:
            errs.append("name %r is a substring of %r -- substring matching unsound" % (a, b))
            break
    fid = {f["id"] for f in facts}
    for c in chosen:
        for s in c["supports"]:
            if s not in fid:
                errs.append("%s: support %s missing" % (c["id"], s))
        ses = [FACT[s]["session"] for s in c["supports"]]
        if len(set(ses)) != len(ses):
            errs.append("%s: supports share a session %s" % (c["id"], ses))
        if len(c["supports"]) != c["hops"]:
            errs.append("%s: %d supports for %d hops" % (c["id"], len(c["supports"]), c["hops"]))
        if one_hop_reaches(c["anchor"], c["gold"]):
            errs.append("%s: ONE HOP REACHES IT" % c["id"])
        # the chain must actually terminate at the gold
        cur = c["anchor"]
        for r in c["rels"]:
            cur = rel[(cur, r)]
        if cur != c["gold"]:
            errs.append("%s: path does not end at the gold" % c["id"])
        # and no single session may carry the whole chain
        if len({FACT[s]["session"] for s in c["supports"]}) < 2:
            errs.append("%s: answerable within one session" % c["id"])
    seen = set()
    for s in session_rows:
        for m in s["fact_ids"]:
            if m in seen:
                errs.append("fact %s in two sessions" % m)
            seen.add(m)
    if len(seen) != len(facts):
        errs.append("%d facts but %d placed in sessions" % (len(facts), len(seen)))
    dia = set()
    for s_ in session_rows:
        for t in s_["turns"]:
            if t["dia_id"] in dia:
                errs.append("duplicate dia_id %s" % t["dia_id"])
            dia.add(t["dia_id"])
    for f in facts:
        if not f.get("turn_dia_id"):
            errs.append("fact %s was never rendered into a turn" % f["id"])
        elif f["turn_dia_id"] not in dia:
            errs.append("fact %s names dia_id %s which no turn has" % (f["id"], f["turn_dia_id"]))
    return errs


errs = validate()
corpus = {
    "note": ("RFC DB-2 synthesis corpus. Deterministic: seeded RNG over templates, so "
             "extraction is the only stochastic part of the benchmark. Entities are "
             "INVENTED so the no-memory arm measures leakage rather than world knowledge. "
             "Every question is a path through a typed graph; hops = facts required. No "
             "question is answerable from one fact or from one session -- both enforced."),
    "seed": SEED,
    "entity_counts": {"people": len(PEOPLE), "orgs": len(ORGS), "cities": len(CITIES),
                      "regions": len(REGIONS), "projects": len(PROJECTS)},
    "facts": [{"id": f["id"], "session": f["session"], "turn_dia_id": f["turn_dia_id"],
               "text": f["text"], "s": f["s"], "r": f["r"], "o": f["o"]} for f in facts],
    "sessions": session_rows,
    "questions": [{"id": c["id"], "hops": c["hops"], "q": c["q"], "gold": c["gold"],
                   "anchor": c["anchor"], "relations": c["rels"],
                   "supports": c["supports"],
                   "guess_floor": round(1.0 / max(1, gold_pool(c["gold"])[1]), 4)}
                  for c in chosen],
}

HERE = os.path.dirname(os.path.abspath(__file__))
out = os.path.join(HERE, "corpus-db2.json")
json.dump(corpus, open(out, "w"), indent=1)

print("facts    %d in %d sessions (max %d/session)" % (len(facts), len(sessions), MAX_FACTS_PER_SESSION))
print("entities %s" % corpus["entity_counts"])
print("candidates %d -> kept %d (dropped %d as one-hop-reachable)" % (len(candidates), len(kept), dropped_one_hop))
hopmix = defaultdict(int)
for c in chosen:
    hopmix[c["hops"]] += 1
print("questions %d  hop mix %s" % (len(chosen), dict(sorted(hopmix.items()))))
floors = [c["guess_floor"] for c in corpus["questions"]]
print("guess floor max %.3f (worst-case blind guess)" % max(floors))
print("VALIDATION: %s" % ("clean" if not errs else "%d PROBLEMS" % len(errs)))
for e in errs[:15]:
    print("  " + e)
print("wrote", out)

"""Study D (the loomcycle development tenant): paths, arms and constants.

Named dconfig, not config, so it never shadows ../config.py, which ../common.py imports
for the shared provider policy and prices (identical for both studies).
"""
import os

HERE = os.path.dirname(os.path.abspath(__file__))
KBRAG = os.path.dirname(HERE)
WORK = os.environ.get("KBRAG_DOCS_WORK", "/home/denn/workspace/kbrag-work/docs")
INVENTORY = os.path.join(WORK, "inventory.json")
EXPORT = os.path.join(WORK, "export")          # one JSON per document: sections with heading paths
SECTIONS = os.path.join(WORK, "sections.json") # corpus sections + chunk -> (document, section) map
RAGEXT = os.path.join(WORK, "ragext")
GOLD = os.path.join(WORK, "gold", "gold.yaml")           # the walker questions' gold (machine-filled)
KNOWN = os.path.join(WORK, "gold", "known_items.json")   # the known-item set (mechanical gold)
WALKER_QUESTIONS = os.path.join(KBRAG, "..", "..", "..", "scripts", "measure", "walker", "questions.yaml")

# The corpus (PREREG check 1): every document of the tenant scope, less the operational
# types and the two untyped system documents under /memory (the deployment context and the
# tenant ontology). Not only /loomcycle: the operator's gold names documents filed under
# /documents and elsewhere too. kb/ask cannot be restricted; a kb citation of a document
# outside the corpus counts as a wrong citation for precision.
CORPUS_ROOT = "/loomcycle"
OPERATIONAL_TYPES = {"Annotation", "SourceStub", "SyncReport", None}

ARMS = ("kb", "rag-lc", "rag-ext")
K = 20                       # sections the reader sees (PREREG, Study D)
REPS = 3                     # runs per question per arm (PREREG, Study D)
RAGLC_AGENT = "kbrag/search-rr"
RAGLC_LIMIT = 40             # search rows asked for, so 20 distinct readable sections remain
READER = ("anthropic", "claude-haiku-5-5")
JUDGE = ("anthropic", "claude-haiku-5-5")
BUDGET_USD = 19.0            # hard stop: increase of the tenant ledger over BASELINE
COST_FLOOR = {"kb": 0.05}
COST_FLOOR_DEFAULT = 0.01
# A section longer than this is cut before the reader sees it (the 99th percentile is about
# 8,700 characters; a few are over 100,000, which would crowd out the other 19).
SECTION_CHARS = 8000

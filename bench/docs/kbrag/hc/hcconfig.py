"""RFC HC gate study: arms, sets, thresholds, seeds and the environment it reads.

Named hcconfig so it never shadows ../config.py, which ../common.py imports for the shared
provider policy (Haiku, any ollama-local model, code-js) and the price list.

Nothing here names a deployment: the base URL, the tenant names and the bearers come from
environment variables whose NAMES are below (override them per run with --base-env,
--token-env, --tenant-env). A bearer's value is never printed, logged or written.
"""
import os

HERE = os.path.dirname(os.path.abspath(__file__))
KBRAG = os.path.dirname(HERE)
MEASURE = os.path.normpath(os.path.join(KBRAG, "..", "measure"))
SUBSET = os.path.join(MEASURE, "reader_subset.json")      # the 800 test questions (RFC DM)
SPLITS = os.path.join(HERE, "splits.json")                 # the frozen dev/test split

# Run files, condensed answers, tenant state and the walker gold stay OUTSIDE the repo.
WORK_ENV = "KBRAG_HC_WORK"


def work():
    w = os.environ.get(WORK_ENV, "")
    if not w:
        raise SystemExit("%s is not set: point it at the study's work directory (outside the repo)" % WORK_ENV)
    return w


# One tenant per set. Every name below is an environment variable NAME, not a value.
TENANTS = {
    "cqa": {"base_env": "KBRAG_HC_CQA_BASE", "token_env": "KBRAG_HC_CQA_TOKEN", "tenant_env": "KBRAG_HC_CQA_TENANT"},
    "p7": {"base_env": "KBRAG_HC_DEV_BASE", "token_env": "KBRAG_HC_DEV_TOKEN", "tenant_env": "KBRAG_HC_DEV_TENANT"},
}
SETS = ("cqa", "p7")

# The arms (RFC HC §6.2 + §11; plan §3.3). rag-ext is not run (owner ruling): T1 is against
# rag-lc only. The oracle (kb/read on the gold sections) exists for instrument checks 6 and 7.
ARMS = ("kb-today", "kb-first", "kb-today+answer-first", "rag-lc", "oracle")
ARMS_FOR = {"cqa": ARMS, "p7": ("kb-today", "kb-first", "kb-today+answer-first", "rag-lc")}
KB_ARMS = ("kb-today", "kb-first", "kb-today+answer-first")
# The agent each arm starts. kb-today+answer-first is kb/ask with an answer-first kb/search;
# a tenant fork of kb/search would shadow it for kb-today too, so the arm runs a COPY of kb/ask
# under its own name that spawns the answer-first search under its own name. Both copies are
# tenant-side setup, recorded with their content_sha256 in Amendment 0. The names are
# overridable so Amendment 0 can record what was actually created.
AGENTS = {
    "kb-today": "kb/ask",
    "kb-first": "kb/ask",
    "kb-today+answer-first": os.environ.get("KBRAG_HC_AF_ASK_AGENT", "kbhc/ask-af"),
    "rag-lc": "kb/answer",
    "oracle": "kb/read",
}
AF_SEARCH_AGENT = os.environ.get("KBRAG_HC_AF_SEARCH_AGENT", "kbhc/search-af")
# Run metadata per arm. kb-first turns the first call on (RFC HC §2.3 flag).
METADATA = {
    "kb-today": {"depth": "standard"},
    "kb-first": {"depth": "standard", "first_call": True},
    "kb-today+answer-first": {"depth": "standard"},
    "rag-lc": {},
    "oracle": {},
}

# Agents whose active tenant def is recorded (content_sha256) before the study and checked
# before every invocation and every 20 runs: a def that changes mid-study stops the runner.
WATCH = ("kb/ask", "kb/classify", "kb/search", "kb/screen", "kb/walker", "kb/survey", "kb/answer", "kb/read",
         AGENTS["kb-today+answer-first"], AF_SEARCH_AGENT)
# Model-backed agents that must resolve to Haiku through a tenant fork (plan §3.2).
HAIKU = ("anthropic", "claude-haiku-5-5")
HAIKU_PINNED = ("kb/classify", "kb/search", "kb/screen", "kb/walker", "kb/read", AF_SEARCH_AGENT)
# Agent-name prefixes whose model-backed runs must be Haiku in every run tree.
PINNED_PREFIXES = ("kb/", "kbhc/")

K = 5                        # sections kb/answer reads by default (D9), and the oracle's cap
P7_REPS = 3                  # runs per question per arm on the walker set
SPREAD_SEED = 20261122       # the fixed 100 of the CQA test set (same sample as the benchmark)
SPREAD_N = 100
SPREAD_REPS = 3              # rep 0 for all 800; reps 1-2 for the fixed 100
ORDER_SEED = 20261127        # seeded question order and arm rotation

# Hypotheses (RFC HC §6.3): three co-primary tests, two-sided alpha 0.05/3 each -> 98.33% CI.
ALPHA = 0.05 / 3
BOOT_N = 10000
BOOT_SEED = 1
T1_BOUND = -0.02             # kb-first - rag-lc, condensed F1, CQA 800
T2_BOUND = 0.0               # kb-first - kb-today, condensed F1, CQA 800
T3_BOUND = -0.10             # kb-first - kb-today, cited-document F1, p7 (21 document-gold questions)
T3_STOP_POINT = -0.05        # plan §5: T3 must not fail with a point estimate below this
COST_CQA_MAX_RATIO = 0.5     # kb-first USD/question <= half of kb-today's
COST_P7_MAX_RATIO = 1.10     # kb-first USD/run <= kb-today's + 10%

# Instrument-check thresholds (PREREG-HC).
COVERAGE_MIN = 0.99
STALE_MAX = 0.01
RERANKED_MIN = 0.95
CONDENSE_MAX_DIFF = 0.03     # to be confirmed in Amendment 0 (the benchmark used 0.02)
RETRIEVAL_X_CHANCE = 10

# Spend: the cap is $20; the runner stops at a $19 increase summed over both tenants' ledgers.
CAP_USD = 20.0
BUDGET_USD = 19.0
COST_FLOOR = {"kb-today": 0.05, "kb-first": 0.05, "kb-today+answer-first": 0.05}
COST_FLOOR_DEFAULT = 0.01
KEY_BASE = "kbrag-hc"

# Corpus filter (check 1; the same list is check 2's denominator).
OPERATIONAL_TYPES = {"SourceStub", "SyncReport", "Annotation"}
MEMORY_ROOT = "/memory"
ONTOLOGY_PATH = "/memory/ontology"

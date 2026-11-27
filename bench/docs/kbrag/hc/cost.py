"""The study's price model: cost() as in the measurement price model of the set-11 runs
(Haiku 5.5: $0.10 / M input, $0.50 / M output, $0.01 / M cache read, $0.125 / M cache
write; local models $0), and the mapping from the keys the server reports onto its names.

cost() takes the Anthropic-style usage names (cache_read_input_tokens,
cache_creation_input_tokens). A loomcycle run row reports cache_read_tokens and
cache_creation_tokens; ../common.Usage holds cached_tokens and cache_creation_tokens.
from_usage() accepts all three spellings, so one function prices every source alike.

../config.price (what ../common.Usage records as usd_est on every row) uses the same four
prices; tests/test_cost.py pins that the two agree on a Haiku row.
"""

# Providers whose calls are free in this study: the deployment's local models, and code-js
# (it makes no model call of its own; its children are priced on their own rows).
FREE_PROVIDERS = {"ollama-local", "code-js", "", None}


def cost(u):
    u = u or {}
    return (u.get('input_tokens', 0) * 0.10 + u.get('output_tokens', 0) * 0.50
            + u.get('cache_read_input_tokens', 0) * 0.01 + u.get('cache_creation_input_tokens', 0) * 0.125) / 1e6


def from_usage(u):
    """Any usage dict -> cost()'s key names. Keys: input_tokens, output_tokens, and the cache
    read as cache_read_input_tokens | cache_read_tokens | cached_tokens, the cache write as
    cache_creation_input_tokens | cache_creation_tokens."""
    u = u or {}

    def first(*names):
        for n in names:
            if u.get(n):
                return u[n]
        return 0

    return {"input_tokens": u.get("input_tokens", 0) or 0, "output_tokens": u.get("output_tokens", 0) or 0,
            "cache_read_input_tokens": first("cache_read_input_tokens", "cache_read_tokens", "cached_tokens"),
            "cache_creation_input_tokens": first("cache_creation_input_tokens", "cache_creation_tokens")}


def run_cost(row):
    """One run row (a GET /v1/runs row, or a tree row of client.tree_usage): $0 for a free
    provider, else cost() of its usage."""
    usage = row.get("usage") if isinstance(row.get("usage"), dict) else row
    provider = usage.get("provider", row.get("provider"))
    if provider in FREE_PROVIDERS:
        return 0.0
    return cost(from_usage(usage))


def tree_cost(tree):
    """USD of a whole run tree (client.tree_usage), summed over the root and its children."""
    return sum(run_cost(r) for r in (tree or {}).get("runs") or [])

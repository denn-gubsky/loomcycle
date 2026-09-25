# Held-out confirmation — written BEFORE the run

Committed before any held-out number exists, so the result cannot be read in whichever
light suits it.

**Set:** QASPER dev v0.3, papers 41–80 of the same seed-7 shuffle (`prepare.py --skip 40`).
40 papers, 120 scoreable questions, 633 section chunks, zero overlap with the development
40. Separate tenant (`qasper-heldout`), same bench server, embedder, reader and prompts.
Nothing is tuned on this set: the prompts, map format, rerank prompt and k are the ones
fixed on the development set.

**Arms:** `header`, `mapall`, `header+rr`, `mapall+rr`. Three reader passes each (seeds 0,
11, 23), per-question F1 averaged over passes, paired bootstrap (4000 resamples, seed 7).

**Primary hypothesis — decides the design:**

- **H1:** `mapall+rr` beats `header` on Answer-F1. It is confirmed only if the lower bound
  of the paired 95% CI is above 0. Development set: +0.091 (+0.035…+0.150).

**Secondary** (each reported whatever it shows):

- **H2:** `header+rr` beats `header` on R@5, exact McNemar p < 0.05. Development set:
  0.671 → 0.870.
- **H3:** `mapall` beats `header` on F1, CI lower bound above 0. Development set: +0.065.
- **H4:** `mapall+rr` beats `mapall` on F1. Development set: +0.026, CI includes 0. With
  120 questions this is expected to stay unresolved; it is reported, not decided.

**What would change the plan:**

- **H1 fails:** the RFC does not propose the map/rerank design as validated. It goes back
  to a larger probe.
- **H1 holds and H3 fails:** the rerank carries the result, and the map is optional in the
  RFC.

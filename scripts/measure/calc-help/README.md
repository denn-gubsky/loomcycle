# calc-help — can a model use Calc from its help alone? (prepared, not run)

The Calc tool's model-facing text is its description, its schema and the help
it can read (`Context op=help topic=Calc`, the `Calc/<section>` articles and
the `calculations` recipes). This measurement asks, per section of the
language: given a plain question, does a model call Calc, is its first call
well formed, how often does it read the help, and is the final answer right?

This directory holds the task set and the driver. It has been dry-run against
the mock provider only; the two real arms below are for the owner to run.

## The task set

`tasks.yaml`: 62 questions written for this measurement — 8 `elementary`
(arithmetic, powers, logarithms, trigonometry), 8 `integers`, 7 `sequences`,
8 `units`, 8 `statistics`, 8 `finance`, 8 `dates` and 7 `word` problems of
several steps. A prompt is a question as a user would put it, never a Calc
expression. Never edit a task after the first run; add a new id.

Every task records its `answer`, a `tolerance` (`exact` for exact quantities
and stated roundings, `abs:<x>` or `rel:<x>` for a float) and a `check`: the
Python expression that computed the answer with `fractions`, `decimal`,
`datetime`, `math` and `statistics` and a few helpers in `test_drive.py`
(round half up, working days, month arithmetic, IRR by bisection). No answer
was read off Calc. `test_drive.py` evaluates every `check` and fails if an
answer differs from it.

## The arms

One agent, `calc-measure` in `loomcycle.yaml`, holds only `Calc` and `Context`
(the `Skill` default is switched off). Its system prompt asks it to use Calc
for every number, to read the help when unsure, and to end with a line
`ANSWER: <value>`. It says nothing else about Calc: what the model knows of
the language comes from the tool's description, schema and help.

The definition pins Haiku. Each run names its provider and model, so both
arms run the same definition:

- **haiku** — `--provider anthropic --model claude-haiku-5-5`.
- **local** — `--provider ollama-local --model <a model your Ollama serves>`,
  e.g. `qwen3.6:latest`.

A pinned agent has no tier cascade, so a run does not move to another
provider part-way; the driver still records the model each run reports and
warns about any run that ran on a different one.

`calc-plumbing` is a code-js stand-in used only by the dry run (below).

## Run

Scripts never read `.env.local`. Load it into the shell yourself first; the
driver reads only the variable names given here and never prints a value.

### 1. Boot a server with this config

```sh
set -a; source .env.local; set +a        # ANTHROPIC_API_KEY, OLLAMA_BASE_URL, ...
make build                               # or go build -o bin/loomcycle ./cmd/loomcycle
export LOOMCYCLE_AUTH_TOKEN="measure-$(date +%s)"   # a bearer for this server only
LOOMCYCLE_DATA_DIR="$(mktemp -d)" \
LOOMCYCLE_LISTEN_ADDR=127.0.0.1:18980 \
LOOMCYCLE_CODE_AGENTS_ROOT=scripts/measure/calc-help/agent_code \
  ./bin/loomcycle --config scripts/measure/calc-help/loomcycle.yaml
```

The `ollama-local` provider is built in and keyless; its endpoint is
`OLLAMA_BASE_URL` (default `http://localhost:11434`). To name another host in
yaml instead, add to a second config file layered after this one
(`--config scripts/measure/calc-help/loomcycle.yaml --config local.yaml`):

```yaml
providers:
  ollama-local:
    driver: ollama
    base_url: http://<your ollama host>:11434
```

### 2. Run an arm (in another shell with the same `LOOMCYCLE_AUTH_TOKEN`)

```sh
export LOOMCYCLE_URL=http://127.0.0.1:18980
OUT=~/calc-help-results/$(date +%Y%m%d-%H%M)          # outside the repo

# haiku
python3 -I scripts/measure/calc-help/drive.py --plan --provider anthropic --model claude-haiku-5-5
python3 -I scripts/measure/calc-help/drive.py \
  --provider anthropic --model claude-haiku-5-5 --max-usd 2 --out "$OUT/haiku"

# local: one model loaded at a time on the Ollama host, one run at a time
python3 -I scripts/measure/calc-help/drive.py \
  --provider ollama-local --model qwen3.6:latest --price free --timeout 900 --out "$OUT/local"
```

The driver sends one run at a time and never two in flight; that is what the
Ollama host needs (it serves one model at a time, and parallel runs on a
local model measure queueing rather than the model). Do not run the two arms
at once against the same Ollama host, and unload any other model there
first.

`--runs N` runs every task N times (task order, then again); `--only
finance,d03` runs a subset by section or id. `--summarize <results.jsonl>`
prints the table of a finished run.

## What it records

`results.jsonl`, one line per run: the answer text and the extracted answer,
whether it is correct, the number of Calc calls, whether the **first** Calc
call parsed and its refusal code, every Calc refusal code, the number of help
reads (`Context op=help`) and their topics, tool calls by name and op, the
provider and model the run reported, tokens (input, output, cache read, cache
write), cost under the price model, the stop reason and any error.

"First call parsed" is false when the first Calc call was refused as
malformed: `syntax`, `arity`, `bad_request`, any `unknown_*` code, or a refusal
from the argument check before Calc ran. A well-formed call that the numbers
refused (`domain`, `division_by_zero`, …) counts as parsed.

The answer is the value of the last `ANSWER:` line. A number is compared
exactly as a fraction (thousands separators, currency symbols, units and a
percent sign are ignored; `a/b` is a fraction) or within the task's tolerance;
a date as `YYYY-MM-DD`; a word case-insensitively; `true`/`yes` and
`false`/`no` for a yes-or-no question. A run with no `ANSWER:` line is wrong.

`summary.md` / `summary.json`: per section and overall — runs, the share of
runs that called Calc, the first-call parse rate (of the runs that called
Calc), accuracy, mean Calc calls, mean help reads, failed runs and spend —
plus the help topics read and the models seen. Results are not committed:
keep `--out` outside the repo.

## Cost (Haiku)

Price model: the one the project's measurements use for Claude Haiku — input
$0.10/M, output $0.50/M, cache read $0.01/M, cache write $0.125/M.

A task is two to five model turns. Assumed per turn: about 4k input tokens
(system prompt, the Calc and Context tool definitions, the conversation, and a
help article when one was read) and 150 output tokens. Uncached, a five-turn
run is about 20k input and 750 output tokens: $0.002 + $0.0004 ≈ **$0.0025 a
run**; prompt caching lowers it. 62 tasks: **about $0.15**; `--runs 3`: about
$0.45. The spend stop assumes a ceiling of $0.02 a run (eight times the
estimate) and stops before a run that could pass `--max-usd` (default $2), so
`--runs 3` (186 runs, ceiling $3.72) needs `--max-usd 4` to be sure of
finishing. The local arm costs no money (`--price free`).

## Dry run (no key, no cost)

Boot the server as above with `LOOMCYCLE_MOCK_ENABLED=1` and no keys, then:

```sh
python3 -I scripts/measure/calc-help/drive.py --agent calc-plumbing --price free --out /tmp/calc-dry/plumbing
python3 -I scripts/measure/calc-help/drive.py --provider mock --model mock-generic --out /tmp/calc-dry/mock
```

`calc-plumbing` (code-js) reads the `calculations` help, makes a refused Calc
call (`2x`) and a good one, and answers `0.3` to every task: expect 100%
called Calc, 0% first call parsed, 2 Calc calls and 1 help read a run, and 0%
accuracy (no task's answer is 0.3). The mock arm exercises the per-run
override and the token accounting; the mock never calls a tool and gives no
`ANSWER:` line.

## Running the tests

```sh
python3 -I -m unittest discover -s scripts/measure/calc-help -p 'test_*.py'
python3 -m pytest scripts/measure/calc-help
```

Standard library only, no network. They check every task's answer against its
`check`, the answer extraction and scoring, the SSE parsing (on event shapes
copied from a real run), the summary and the spend stop.

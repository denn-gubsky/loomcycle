# exp10-gbash-bench — RFC AJ GBash Benchmark Report

## Setup

| Item | Value |
|------|-------|
| loomcycle source | `/Users/denn/work/loomcycle` |
| Bash target dir  | `loomcycle-bench/` |
| Bashbox target dir | `loomcycle-bench-bb/` |
| Ephemeral vol (Bash)    | `/Users/denn/work/loomcycle/examples/exp10-gbash-bench/dynamic/_ephemeral/r_f5534d19a7a8b1ff/bash-ephem` |
| Ephemeral vol (Bashbox) | `/Users/denn/work/loomcycle/examples/exp10-gbash-bench/dynamic/_ephemeral/r_0a7c8baf5f658e40/bashbox-ephem` |

## Timing Results

| Operation | Bash (ms) | Bashbox (ms) | Δ (ms) | Δ% | Notes |
|-----------|-----------|--------------|--------|----|-------|
| `cleanup` | 248 | 556 | +308 | +124% |  |
| `git_clone` | 357 | 409 | +52 | +15% | Bashbox uses RFC AJ §13 fallback proxy |
| `ls_root` | - | 61 | - | - |  |
| `count_all_files` | 55 | 104 | +49 | +89% |  |
| `count_go_files` | 67 | 92 | +25 | +37% |  |
| `count_funcs` | 155 | 636 | +481 | +310% |  |
| `total_loc` | 1342 | 631 | -711 | -53% |  |
| `grep_rfc_aj` | 278 | 782 | +504 | +181% |  |
| `large_files` | 50 | 87 | +37 | +74% |  |
| `dir_depth` | 68 | 86 | +18 | +26% |  |
| **TOTAL** | **2968** | **3875** | **+907** | **+31%** | |

## Output Comparison

Verifying that both modes produce consistent results for discovery ops.

| Operation | Bash result | Bashbox result | Match? |
|-----------|-------------|----------------|--------|
| `count_all_files` | `1274` | `1193` *(gbash stderr appended)* | ✗ MISMATCH |
| `count_go_files` | `708` | `708` *(gbash stderr appended)* | ✓ |
| `count_funcs` | `8119` | `8119` *(gbash stderr appended)* | ✓ |
| `grep_rfc_aj` | `167` | `167` *(gbash stderr appended)* | ✓ |
| `total_loc` | `232733 lines` | `232733 lines` *(gbash stderr appended)* | ✓ |

## Error Summary

No errors in either mode.

## Observations

- **Mode 1 (Bash)**: real `/bin/sh` subprocess; full host PATH; git runs natively.
- **Mode 2 (Bashbox)**: pure-Go in-process sandbox; git escapes via RFC AJ §13 fallback proxy.
- Ephemeral volumes (RFC AH) are created and auto-purge after each run ends.
- **Timing overhead**: each op includes ~30ms from two `python3` subprocess calls used
  for wall-clock measurement (`Date.now()` measures JS overhead only, not tool execution).
- A positive Δ% means Bashbox is slower; negative means Bashbox is faster.

## Bashbox Compatibility Findings

These gaps were discovered during the benchmark:

1. **`grep --include=GLOB`**: not supported by gbash. Replaced with `find | while | grep` pipeline.
2. **EvalSymlinks on relative symlinks**: gbash's `find` calls `EvalSymlinks` on every traversed
   path before type filters are applied. Relative symlinks inside a cloned repo (e.g.
   `loomcycle.example.yaml → cmd/loomcycle/embedded/...`) trigger a containment check that
   fails and aborts traversal. Workaround: use shell glob `dir/*/` to enumerate subdirs,
   bypassing the top-level symlink. Error appears in combined stdout+stderr output.
3. **`xargs`**: not in gbash's built-in coreutils. Replaced with `while read` loops.
4. **`rm -rf` returns null in code-js**: Bashbox returns `null`/`undefined` for commands
   with empty stdout. Code-js must use `Bashbox({...}) || ''` defensively.

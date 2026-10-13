# schedprobe — RFC EI P0 run book

What the experiments decide, and the rules that decide it, are in [PREREG.md](PREREG.md).
This file is only how to run them.

```sh
go build -o bin/schedprobe ./bench/cmd/schedprobe
export OLLAMA_BASE_URL=http://<host>:11434
CHAT=<chat model>; CTX=<num_ctx>; mkdir -p bench/docs/schedprobe/runs; R=bench/docs/schedprobe/runs
```

The host must be idle. Note its `OLLAMA_NUM_PARALLEL` (N) and `OLLAMA_MAX_LOADED_MODELS`
in the label, and restart Ollama when E2 changes N.

## Workloads

```sh
bin/schedprobe -gen -agents 1 -model $CHAT -num-ctx $CTX            > /tmp/w1.jsonl
for k in 2 4 6 8; do
  bin/schedprobe -gen -agents $k -model $CHAT -num-ctx $CTX         > /tmp/w$k.jsonl
done
bin/schedprobe -gen -agents 4 -model $CHAT -num-ctx $CTX \
  -embed-agents 2 -embed-model <embedder> -decision-agents 1 -decision-model <decision model> > /tmp/w-mixed.jsonl
bin/schedprobe -gen -agents 0 -embed-agents 2 -embed-model <embedder> \
  -decision-agents 1 -decision-model <decision model> -agent-prefix s  > /tmp/w-solo.jsonl
bin/schedprobe -gen -agents 2 -model $CHAT -num-ctx 8192,$CTX       > /tmp/w-ctx.jsonl
```

## Runs

```sh
# E1 — one agent, cold and warm
bin/schedprobe -workload /tmp/w1.jsonl -policy none -cold -label E1-cold -out $R/E1-cold.jsonl
bin/schedprobe -workload /tmp/w1.jsonl -policy none       -label E1-warm -out $R/E1-warm.jsonl

# E2 — per call, no hold; repeat for host N=2 and N=4
for k in 2 4 6 8; do
  bin/schedprobe -workload /tmp/w$k.jsonl -policy call -slots chat=$k -label E2-n$N-k$k -out $R/E2-n$N-k$k.jsonl
done

# E3 — 8 agents, host N=4, probe chat slots 4
bin/schedprobe -workload /tmp/w8.jsonl -policy none -label E3-none -out $R/E3-none.jsonl
bin/schedprobe -workload /tmp/w8.jsonl -policy run  -slots chat=4 -label E3-run  -out $R/E3-run.jsonl
bin/schedprobe -workload /tmp/w8.jsonl -policy call -slots chat=4 -label E3-call -out $R/E3-call.jsonl
for q in 1 2 4 8; do for h in 0 2000 5000; do
  bin/schedprobe -workload /tmp/w8.jsonl -policy lease -quantum $q -hold-ms $h -slots chat=4 \
    -label E3-q$q-h$h -out $R/E3-q$q-h$h.jsonl
done; done

# E4 — lanes under load, and the same embed/decision agents alone
bin/schedprobe -workload /tmp/w-solo.jsonl  -policy call -slots chat=4,embed=2,decision=1 -label E4-solo  -out $R/E4-solo.jsonl
bin/schedprobe -workload /tmp/w-mixed.jsonl -policy call -slots chat=4,embed=2,decision=1 -label E4-mixed -out $R/E4-mixed.jsonl

# E5 — one model at two num_ctx values
bin/schedprobe -workload /tmp/w-ctx.jsonl -policy call -slots chat=1 -label E5 -out $R/E5.jsonl
```

## Tables

```sh
python3 bench/docs/schedprobe/analyze.py --cold $R/E1-cold.jsonl --warm $R/E1-warm.jsonl $R/E2-*.jsonl $R/E3-*.jsonl $R/E4-*.jsonl $R/E5.jsonl
```

A record per call is appended to `-out`, so a re-run under the same label must write a
fresh file. A run interrupted with Ctrl-C keeps the calls it finished.

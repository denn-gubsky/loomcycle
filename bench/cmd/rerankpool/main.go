// Command rerankpool reranks pre-fetched candidate pools with the SHIPPED listwise
// rerank: the operator's memory.reranker block, built by reranker.Build, asked through
// memory.RerankTexts — the measured prompt and the reply repair, unchanged.
//
// It exists because the shipped rerank only runs inside a document search, and a
// benchmark that compares rerankers over the same pools (memory rows, say) needs the
// baseline to be the real code path, not a re-implementation of its prompt.
//
//	rerankpool -config reranker.yaml -in pools.jsonl -out qwen_list.jsonl [-max-chars 1200]
//
// pools.jsonl: one {"qid", "query", "keys": [...], "texts": [...]} per line.
// Output: one {"qid", "order": [keys], "reranked", "rerank_reason", "ms"} per line.
// Resumable: qids already in -out are skipped.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/reranker"
)

type pool struct {
	QID   string   `json:"qid"`
	Query string   `json:"query"`
	Keys  []string `json:"keys"`
	Texts []string `json:"texts"`
}

type result struct {
	QID          string   `json:"qid"`
	Order        []string `json:"order"`
	Reranked     bool     `json:"reranked"`
	RerankReason string   `json:"rerank_reason,omitempty"`
	MS           int64    `json:"ms"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "rerankpool:", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("config", "", "yaml holding the memory.reranker block")
	in := flag.String("in", "", "pools jsonl")
	out := flag.String("out", "", "results jsonl (appended)")
	maxChars := flag.Int("max-chars", memory.DefaultRerankMaxChars, "per-candidate characters, as memory_rerank.max_chars")
	flag.Parse()
	if *cfgPath == "" || *in == "" || *out == "" {
		return fmt.Errorf("-config, -in and -out are required")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	model, err := reranker.Build(cfg)
	if err != nil {
		return err
	}
	if model == nil {
		return fmt.Errorf("%s declares no memory.reranker", *cfgPath)
	}
	done := map[string]bool{}
	if f, err := os.Open(*out); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for sc.Scan() {
			var r result
			if json.Unmarshal(sc.Bytes(), &r) == nil {
				done[r.QID] = true
			}
		}
		f.Close()
	}
	src, err := os.Open(*in)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(*out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer dst.Close()
	sc := bufio.NewScanner(src)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	n := 0
	start := time.Now()
	for sc.Scan() {
		var p pool
		if err := json.Unmarshal(sc.Bytes(), &p); err != nil {
			return err
		}
		if done[p.QID] {
			continue
		}
		t0 := time.Now()
		order, rep := memory.RerankTexts(context.Background(), model, p.Query, p.Texts, *maxChars)
		r := result{QID: p.QID, Reranked: rep.Applied, RerankReason: rep.Reason, MS: time.Since(t0).Milliseconds()}
		for _, i := range order {
			r.Order = append(r.Order, p.Keys[i])
		}
		b, _ := json.Marshal(r)
		if _, err := dst.Write(append(b, '\n')); err != nil {
			return err
		}
		n++
		if n%50 == 0 {
			fmt.Printf("qwen_list %d %.2fs/q\n", n, time.Since(start).Seconds()/float64(n))
		}
	}
	fmt.Printf("done qwen_list %d\n", n)
	return sc.Err()
}

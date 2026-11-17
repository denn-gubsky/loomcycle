// Command nodestats is the RFC GM P0 measurement: it reads a restored dump of
// one tenant's documents, annotation vectors and classification statements and
// reports how well the declared taxonomy agrees with the vectors, what each node
// looks like, where it would split, and how documents are dated. It is a
// standalone program; the runtime does not import it. See README.md.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "nodestats:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		mainDSN    = flag.String("main-dsn", os.Getenv("GM_P0_MAIN_DSN"), "runtime database DSN (memory, memory_embeddings); default $GM_P0_MAIN_DSN")
		sqlDSN     = flag.String("sqlmem-dsn", os.Getenv("GM_P0_SQLMEM_DSN"), "SQL Memory database DSN; default $GM_P0_SQLMEM_DSN")
		tenant     = flag.String("tenant", "", "tenant id (required)")
		docSchema  = flag.String("doc-schema", "", "document schema; empty finds the tenant scope's in sqlmem_meta")
		schemeRoot = flag.String("scheme-root", "", "root chunk id of the scheme to measure (required)")
		kinds      = flag.String("subject-kinds", "subject", "comma-separated statement kinds that place a document on a concept")
		bodyScope  = flag.String("body-scope-id", "", "memory scope_id of the tenant scope's chunk bodies")
		vecScope   = flag.String("vector-scope-id", "doc.annotations", "memory scope_id of the annotation vectors")
		model      = flag.String("model", "bge-m3:latest", "embedding model of the vectors (and of the definition baseline)")
		embedURL   = flag.String("embed-url", "http://localhost:11434/api/embed", "Ollama /api/embed URL for the definition baseline; empty skips it")
		promptV    = flag.Int("current-prompt-version", 2, "the current annotation prompt version")
		perms      = flag.Int("permutations", 50, "label permutations for the baseline")
		seeds      = flag.Int("split-seeds", 10, "Split seeds 1..n")
		margins    = flag.String("misfit-margins", "0,0.02,0.05,0.1", "comma-separated misfit margins")
		out        = flag.String("out", "", "output directory (required)")
		imported   = flag.String("imported-until", "", "last day (YYYY-MM-DD, UTC) of the corpus's bulk import; documents created on or before it are dated by the import")
	)
	flag.Parse()
	if *mainDSN == "" || *sqlDSN == "" || *tenant == "" || *schemeRoot == "" || *out == "" {
		flag.Usage()
		return fmt.Errorf("need --main-dsn, --sqlmem-dsn (or their env vars), --tenant, --scheme-root and --out")
	}
	ms, err := parseFloats(*margins)
	if err != nil {
		return fmt.Errorf("--misfit-margins: %w", err)
	}

	ctx := context.Background()
	mainDB, err := pgx.Connect(ctx, *mainDSN)
	if err != nil {
		return fmt.Errorf("connect main database: %w", err)
	}
	defer mainDB.Close(ctx)
	sqlDB, err := pgx.Connect(ctx, *sqlDSN)
	if err != nil {
		return fmt.Errorf("connect SQL Memory database: %w", err)
	}
	defer sqlDB.Close(ctx)

	src := Source{Tenant: *tenant, DocSchema: *docSchema, SchemeRoot: *schemeRoot,
		SubjectKinds: strings.Split(*kinds, ","), BodyScopeID: *bodyScope, VectorScopeID: *vecScope, Model: *model}
	fmt.Fprintln(os.Stderr, "loading…")
	data, err := Load(ctx, mainDB, sqlDB, src)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "loaded %d documents, %d concepts, %d live statements\n",
		len(data.Docs), len(data.Concepts), len(data.Statements))

	defVecs, defNote := EmbedDefinitions(ctx, *embedURL, *model, data.Concepts)
	if defVecs != nil && !sameDim(defVecs, data) {
		defVecs, defNote = nil, "definition baseline skipped: definition vectors differ in dimension from the stored ones"
	}
	fmt.Fprintln(os.Stderr, defNote)

	opt := Options{CurrentPromptVersion: *promptV, Permutations: *perms, SplitSeeds: *seeds,
		MisfitMargins: ms, NonCurrentLimit: 0.10}
	if *imported != "" {
		day, err := time.Parse("2006-01-02", *imported)
		if err != nil {
			return fmt.Errorf("--imported-until: %w", err)
		}
		opt.ImportedUntil = day
	}
	res, err := Measure(data, opt, defVecs, defNote)
	if err != nil {
		return err
	}
	return write(*out, res)
}

func write(dir string, res *Result) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	j, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "nodestats.json"), j, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "nodestats.md"), []byte(Render(res)), 0o644); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "wrote", filepath.Join(dir, "nodestats.json"), "and nodestats.md")
	return nil
}

func sameDim(defVecs map[string][]float64, d *Data) bool {
	for _, doc := range d.Docs {
		if doc.DocUnit == nil {
			continue
		}
		for _, v := range defVecs {
			return len(v) == len(doc.DocUnit)
		}
	}
	return true
}

func parseFloats(s string) ([]float64, error) {
	var out []float64
	for _, p := range strings.Split(s, ",") {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

package eval

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// bundledDataset is the ~20-tuple seed corpus + queries that ships in the
// binary, so `loomcycle memory-eval --dataset bundled` runs with no
// external file. It exercises the metric shapes (conversational / factual
// / multi-session / dedup) but is NOT a benchmark — operators extend it
// via --dataset <file.jsonl>. See seed.jsonl for the schema in practice.
//
//go:embed seed.jsonl
var bundledDataset []byte

// BundledDataset parses the embedded seed dataset.
func BundledDataset() (Dataset, error) {
	return LoadJSONL(bytes.NewReader(bundledDataset))
}

// LoadJSONL parses the harness's JSONL schema:
//
//	line 1         : the CORPUS object — {"name":..,"corpus":[..],"top_k":..,
//	                 "rank":{..}?, "dedup":{..}?}
//	lines 2..N     : one QUERY object each — {"query":"..","expected":["k1",..]}
//
// Splitting corpus (once) from queries (many) keeps each query line tiny
// and lets operators append queries without restating the corpus. Blank
// lines and lines starting with '#' are ignored (comments / spacing).
func LoadJSONL(r io.Reader) (Dataset, error) {
	sc := bufio.NewScanner(r)
	// Allow long corpus lines (the corpus object is the big one).
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var ds Dataset
	gotHeader := false
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !gotHeader {
			// The header line carries name/corpus/top_k/rank/dedup. We
			// decode into Dataset directly; its Queries field stays empty
			// here (queries come from subsequent lines).
			if err := json.Unmarshal([]byte(line), &ds); err != nil {
				return Dataset{}, fmt.Errorf("eval dataset: line %d (corpus header): %w", lineNo, err)
			}
			ds.Queries = nil // ignore any queries embedded in the header line
			gotHeader = true
			continue
		}
		var q Query
		if err := json.Unmarshal([]byte(line), &q); err != nil {
			return Dataset{}, fmt.Errorf("eval dataset: line %d (query): %w", lineNo, err)
		}
		ds.Queries = append(ds.Queries, q)
	}
	if err := sc.Err(); err != nil {
		return Dataset{}, fmt.Errorf("eval dataset: read: %w", err)
	}
	if !gotHeader {
		return Dataset{}, fmt.Errorf("eval dataset: empty or missing corpus header line")
	}
	if len(ds.Corpus) == 0 {
		return Dataset{}, fmt.Errorf("eval dataset: corpus is empty")
	}
	if len(ds.Queries) == 0 {
		return Dataset{}, fmt.Errorf("eval dataset: no query lines")
	}
	return ds, nil
}

package store

import "encoding/json"

// BackfillSystemPromptBase is the JSON-layer transform shared by the SQLite +
// Postgres backfill methods. Returns (newDef, true, nil) when the row needed a
// fill, (nil, false, nil) when it didn't, and (nil, false, err) on a JSON parse
// failure.
//
// Hoisted here (exp7 cleanup) so both adapters call one implementation instead
// of maintaining byte-identical copies — the transform is pure JSON, with no
// adapter-specific dependency, so it belongs in the shared store package.
func BackfillSystemPromptBase(def []byte) ([]byte, bool, error) {
	var raw map[string]any
	if err := json.Unmarshal(def, &raw); err != nil {
		return nil, false, err
	}
	existing, _ := raw["system_prompt_base"].(string)
	if existing != "" {
		return nil, false, nil
	}
	sp, _ := raw["system_prompt"].(string)
	if sp == "" {
		// No system_prompt either — nothing to backfill from. Leave the row
		// as-is; the read-side normalizer is a no-op too.
		return nil, false, nil
	}
	raw["system_prompt_base"] = sp
	out, err := json.Marshal(raw)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

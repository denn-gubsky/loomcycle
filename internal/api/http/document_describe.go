package http

// document_describe.go — POST /v1/_document/describe_images (RFC BU phase 4b).
//
// Generates a vision description for image chunks that have none, persists it next
// to the asset, and re-embeds the chunk so the description reaches the index.
//
// WHY AN OPERATOR ACTION RATHER THAN A WRITE-PATH HOOK. RFC BU requires description
// generation to be asynchronous and best-effort, and the reason is concrete: a 24.9
// GB local vision model returning `unexpected EOF` on cold load is a failure this
// project has already hit. If set_asset awaited a description, that failure would
// reject an author's upload. So set_asset stores bytes and stops; an image with no
// description is unsearchable-by-description but never lost, and phase 4a means it
// is still searchable by its caption in the meantime.
//
// WHY NOT AN IN-PROCESS QUEUE + WORKER. It would be more machinery than the job
// needs, and this codebase already has the right precedent twice over: the phase-2
// embedding backfill and the memory consolidator are both explicit passes rather
// than daemons. An operator (or a Schedule pointed at this route) decides when to
// spend vision calls, which is the same reason the backfill is not a boot migration
// — thousands of model calls nobody approved is a surprise bill.
//
// TIER, NOT A PINNED MODEL. The vision model is resolved through the tier policy
// (default `middle`), so an operator changes it in one place instead of having a
// model name baked into the runtime. A resolved model that cannot see is refused
// BEFORE any call, mirroring the RFC AT pre-call vision gate.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// describeImagesDefaultTier is where the vision model is looked up. `middle` is the
// RFC's choice: describing a picture in two sentences does not need the top tier,
// and a local model at middle keeps the pass free.
const describeImagesDefaultTier = "middle"

// describeMaxTokens bounds the description.
//
// It is a CEILING, not a target: a model stops when it has answered, so a generous
// value costs nothing on a normal call. It is generous because of a failure measured
// on the reference deployment — a thinking-capable vision model (qwen3.6) emits its
// reasoning trace FIRST, and at 300 tokens the trace consumed the entire budget:
// done_reason=length, eval_count=300, and ZERO characters of description. With
// thinking off the same request answers in 84 tokens, so 300 looked ample and was
// not.
//
// A budget rather than forcing `effort: low` (which the Ollama driver maps to
// think:false) DELIBERATELY: that flag errors on a model which cannot reason, so
// forcing it would break a non-thinking vision model such as llava to accommodate a
// thinking one. A ceiling is model-agnostic.
const describeMaxTokens = 1500

// describeCallTimeout bounds ONE vision call, not the sweep. A cold local vision
// model can take tens of seconds on its first request, so it is generous. The
// sweep's own bound is `limit`, which is what an operator tunes to keep a single
// invocation short.
const describeCallTimeout = 5 * time.Minute

const describeSampleCap = 20

// errNoVisionResolver is the SERVICE-level absence (no resolver at all) as opposed
// to a misconfigured tier. They get different statuses on purpose: 503 says "this
// deployment cannot do this", 400 says "this tier can't, try another" — and only the
// first is worth retrying unchanged.
var errNoVisionResolver = errors.New("no resolver configured; image description needs tier resolution")

type describeImagesResponse struct {
	Scope    string `json:"scope"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Tier     string `json:"tier"`
	DryRun   bool   `json:"dry_run"`

	// Candidates is how many undescribed assets this call SAW, bounded by limit.
	Candidates int `json:"candidates"`
	Described  int `json:"described,omitempty"`
	Failed     int `json:"failed,omitempty"`
	// Empty counts images the model looked at and produced nothing usable for. They
	// are marked examined so a re-run does not keep paying for them.
	Empty int `json:"empty,omitempty"`

	Samples      []describeSample `json:"samples,omitempty"`
	FailedChunks []string         `json:"failed_chunks,omitempty"`
	FirstFailure string           `json:"first_failure,omitempty"`
	Notes        []string         `json:"notes"`
}

type describeSample struct {
	ChunkID     string `json:"chunk_id"`
	MediaType   string `json:"media_type"`
	Size        int    `json:"size"`
	Caption     string `json:"caption,omitempty"`
	Description string `json:"description,omitempty"`
}

// handleDescribeImages serves POST
// /v1/_document/describe_images?scope=&scope_id=&tenant=&tier=&limit=&dry_run=
//
// scope_id/tenant are resolved through substrateBrowseCtxFn, the same authorization
// path the Path/Document browse routes use: an admin may target any tenant, a
// substrate:tenant operator only its own, and neither may be supplied by a model.
func (s *Server) handleDescribeImages(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.sqlMem == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "sqlmem_unavailable",
			"image descriptions require SQL Memory (set LOOMCYCLE_SQLMEM_ENABLED=1)")
		return
	}
	// RFC AX (fail-closed), for the same reason the LLM gateway refuses here: this
	// route calls provider.Call DIRECTLY and does not wire the RFC AR credential
	// override, so a restricted principal has no way to bring its own key and would
	// spend the operator's. Both run-path enforcement layers are on the agent-run
	// path, not this one, so refuse at the choke point. Gate-off ⇒ always false ⇒
	// byte-identical for every existing token.
	if s.operatorKeyRestrictedForCtx(r.Context()) {
		writeJSONError(w, http.StatusForbidden, "operator_key_restricted", operatorKeyRestrictedMsg)
		return
	}

	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = "user"
	}
	if scope != "agent" && scope != "user" && scope != "tenant" {
		writeJSONError(w, http.StatusBadRequest, "invalid_scope",
			"scope must be one of: agent, user, tenant")
		return
	}
	// dry_run defaults TRUE, matching every other sweep here: a bare `curl -X POST`
	// must not spend a vision call per image.
	dryRun := true
	if v := r.URL.Query().Get("dry_run"); v != "" {
		dryRun = v != "false" && v != "0"
	}
	limit := 25
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	tier := r.URL.Query().Get("tier")
	if tier == "" {
		tier = describeImagesDefaultTier
	}

	ctx := s.substrateBrowseCtxFn(r)(r.Context())
	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem, Embedder: s.embedder}

	assets, err := doc.ListUndescribedAssets(ctx, scope, limit)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}

	resp := describeImagesResponse{
		Scope: scope, Tier: tier, DryRun: dryRun, Candidates: len(assets),
	}
	resp.Notes = []string{
		"candidates is bounded by limit — a non-zero count after a live run means MORE " +
			"remain; re-invoke until it reaches 0. Every described asset leaves the " +
			"candidate set, so re-invoking resumes rather than restarts.",
	}
	if s.embedder == nil {
		resp.Notes = append(resp.Notes,
			"NO EMBEDDER is configured, so a generated description is persisted but NOT "+
				"indexed — it will not make the image searchable until an embedder exists "+
				"and /v1/_memory/backfill_embeddings runs.")
	}

	// Resolve the vision model even for a dry run: an operator planning a sweep needs
	// to learn it would be refused BEFORE they set dry_run=false.
	providerID, prov, model, err := s.resolveVisionModel(tier)
	if err != nil {
		// resolveErrorToStatus maps an unresolvable TIER to 503 (retryable) and
		// anything else to 400; a resolved-but-blind model is the 400 case, since the
		// caller can fix it with ?tier=. No resolver at all is service-level.
		status := resolveErrorToStatus(err)
		if errors.Is(err, errNoVisionResolver) {
			status = http.StatusServiceUnavailable
		}
		writeJSONError(w, status, "vision_model_unavailable", err.Error())
		return
	}
	resp.Provider, resp.Model = providerID, model

	if dryRun {
		for i, a := range assets {
			if i == describeSampleCap {
				break
			}
			resp.Samples = append(resp.Samples, describeSample{
				ChunkID: a.ChunkID, MediaType: a.MediaType, Size: a.Size, Caption: a.Caption,
			})
		}
		resp.Notes = append(resp.Notes,
			"DRY RUN — no vision calls were made and nothing was written. Re-send with dry_run=false.")
		writeJSON(w, http.StatusOK, resp)
		return
	}

	fail := func(chunkID string, err error) {
		resp.Failed++
		if len(resp.FailedChunks) < describeSampleCap {
			resp.FailedChunks = append(resp.FailedChunks, chunkID)
		}
		// One failure message, not one per image: a systematic fault (model down)
		// would otherwise return the same sentence dozens of times, and the FIRST
		// one is the diagnostic.
		if resp.FirstFailure == "" && err != nil {
			resp.FirstFailure = err.Error()
		}
	}

	for _, a := range assets {
		mediaType, raw, ok, err := doc.ReadAsset(ctx, scope, a.ChunkID)
		if err != nil || !ok || len(raw) == 0 {
			if err == nil {
				err = errors.New("asset bytes missing")
			}
			fail(a.ChunkID, err)
			continue
		}
		if mediaType == "" {
			mediaType = a.MediaType
		}
		desc, err := s.describeOneImage(ctx, providerID, prov, model, mediaType, raw, a.Caption)
		if err != nil {
			// NOT marked examined: a transport or model failure is transient, and
			// stamping described_at would silently retire the image from every future
			// pass. Only a model that ANSWERED gets to close the question.
			fail(a.ChunkID, err)
			continue
		}
		if err := doc.SetAssetDescription(ctx, scope, a.ChunkID, desc); err != nil {
			fail(a.ChunkID, err)
			continue
		}
		if desc == "" {
			resp.Empty++
		} else {
			resp.Described++
		}
		if len(resp.Samples) < describeSampleCap {
			resp.Samples = append(resp.Samples, describeSample{
				ChunkID: a.ChunkID, MediaType: mediaType, Size: len(raw),
				Caption: a.Caption, Description: desc,
			})
		}
	}
	if resp.Failed > 0 {
		resp.Notes = append(resp.Notes,
			"some images failed (see failed_chunks, capped; first_failure is the diagnostic). "+
				"They were NOT marked examined, so a re-invocation retries exactly those — a "+
				"transient model failure never silently retires an image.")
	}
	if resp.Empty > 0 {
		resp.Notes = append(resp.Notes,
			"some images were examined but produced no usable description; they are marked "+
				"examined so a re-run does not keep paying for them, and stay searchable by caption.")
	}
	writeJSON(w, http.StatusOK, resp)
}

// resolveVisionModel picks the provider+model for the pass and refuses one that
// cannot see.
//
// The refusal is BEFORE any call, mirroring the RFC AT gate: a text-only model
// handed an image returns an opaque provider 400, and a sweep that turned that into
// "failed" per image would read as a network problem rather than a misconfigured
// tier.
func (s *Server) resolveVisionModel(tier string) (string, providers.Provider, string, error) {
	if s.resolver == nil {
		return "", nil, "", errNoVisionResolver
	}
	dec, err := s.resolver.Resolve(resolve.AgentRequest{Name: "document_describe_images", Tier: tier})
	if err != nil {
		return "", nil, "", err
	}
	prov, err := s.providers.Get(dec.Provider)
	if err != nil {
		return "", nil, "", err
	}
	if !prov.Capabilities().SupportsVision {
		return "", nil, "", fmt.Errorf(
			"tier %q resolves to %s/%s, which cannot accept images — point the tier at a "+
				"vision-capable model, or pass ?tier= one that is",
			tier, dec.Provider, dec.Model)
	}
	return dec.Provider, prov, dec.Model, nil
}

// describeOneImage makes the single vision call.
//
// Returns ("", nil) when the model answered with nothing usable — a DIFFERENT
// outcome from an error, and the caller relies on the distinction: an answered-empty
// image is marked examined, a failed one is left for the next pass.
func (s *Server) describeOneImage(ctx context.Context, providerID string, prov providers.Provider,
	model, mediaType string, raw []byte, caption string) (string, error) {

	// Respect the per-provider concurrency cap (RFC BF P2b) per CALL rather than
	// around the sweep: holding one slot for an entire multi-minute sweep would
	// starve real traffic behind a maintenance pass.
	release, err := s.providerGates.Acquire(ctx, providerID)
	if err != nil {
		return "", err
	}
	defer release()

	// The caption is given to the model so it can add what the author did NOT say.
	// Without it a describe pass tends to restate the caption, which adds tokens to
	// the embedding without adding any new way to find the image.
	instruction := "Describe this image for a search index in one to three sentences. " +
		"State concretely what it shows: the kind of thing it is, any visible text, and " +
		"the objects or UI elements present. Do not speculate about intent, and do not " +
		"preface your answer."
	if caption != "" {
		instruction += " The author's caption is " + strconv.Quote(caption) +
			" — do not restate it; add what it leaves out."
	}

	req := providers.Request{
		Model:     model,
		MaxTokens: describeMaxTokens,
		Messages: []providers.Message{{
			Role: "user",
			Content: []providers.ContentBlock{
				// Data is raw base64 with NO data: prefix — each driver serializes it
				// into its own wire form (RFC AT).
				{Type: "image", MediaType: mediaType, Data: base64.StdEncoding.EncodeToString(raw)},
				{Type: "text", Text: instruction},
			},
		}},
	}

	callCtx, cancel := context.WithTimeout(ctx, describeCallTimeout)
	defer cancel()

	ch, err := prov.Call(callCtx, req)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	var callErr error
	stopReason := ""
	// Drain to completion even after an error: abandoning the channel leaves the
	// driver's goroutine blocked on a send.
	for ev := range ch {
		switch ev.Type {
		case providers.EventText:
			b.WriteString(ev.Text)
		case providers.EventDone:
			if ev.StopReason != "" {
				stopReason = ev.StopReason
			}
		case providers.EventError:
			if callErr == nil {
				callErr = errors.New(ev.Error)
			}
		}
	}
	if callErr != nil {
		return "", callErr
	}
	out := strings.TrimSpace(b.String())
	// EMPTY BECAUSE TRUNCATED IS A FAILURE, NOT AN EMPTY ANSWER, and the distinction
	// decides whether the image is ever looked at again: an empty answer is stamped
	// examined and never retried. A run that hit the token ceiling produced nothing
	// because it ran out of room — on a thinking model the reasoning trace comes
	// first and can consume the whole budget — which is a configuration problem the
	// operator can fix, so it must stay retryable. Recording it as "the model had
	// nothing to say" is how a code bug becomes a permanent fact about the data.
	if out == "" && stopReason == "max_tokens" {
		return "", fmt.Errorf("the model hit the %d-token ceiling before producing any "+
			"description (on a thinking model the reasoning trace is emitted first and can "+
			"consume the whole budget); not marked examined, so it will be retried",
			describeMaxTokens)
	}
	return out, nil
}

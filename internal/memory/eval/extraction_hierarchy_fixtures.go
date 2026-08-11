package eval

// extraction_hierarchy_fixtures.go — the corpus that measures whether a model uses a
// type HIERARCHY correctly (RFC BZ §8).
//
// WHY A SEPARATE CORPUS instead of cases added to the canonical one. The baseline keys
// on the corpus digest, so appending to the shipped fixture would expire the gate on
// every recorded entry at once and demand a re-measurement of all of them. A distinct
// corpus gets distinct keys: the existing numbers stay valid and comparable, and these
// cases accumulate their own history.
//
// It also has to be separate for a correctness reason. These cases can only be scored
// when the rendered ontology actually CONTAINS the hierarchy — run them against the
// flat seed and you are asking the model to choose a subtype it was never shown, and
// scoring it for failing. The harness cannot enforce that pairing, so the invocation
// carries it:
//
//	loomcycle memory-eval-live --provider ollama-local --model qwen3.6:latest \
//	  --corpus hierarchy --ontology-terms "event,event/incident,event/incident/outage,person"
//
// WHAT IT MEASURES, in both directions, because a corpus that measured one would be
// worse than none:
//
//   - under-specifying wastes the taxonomy — the operator classified incidents and gets
//     everything back as `event`.
//   - over-specifying FABRICATES precision, and is the more dangerous direction. A
//     birthday party typed `incident` is a claim about what happened that nobody made,
//     and subtype-expanded retrieval will keep surfacing it under a filter it does not
//     belong to. The RFC's own risk note calls this out: given a ladder, a model tends
//     to climb it further than the evidence goes.
//
// HOW TO READ A MISS, and this matters because the extractor's `type` is OPTIONAL by
// design ("emit both or neither, and only when the fact is clearly about one named
// thing"). A miss here means one of two things, and the report's `typed` column tells
// them apart:
//
//   - typed high → the model emitted types and chose the wrong rung. That is the
//     specificity failure this ability names.
//   - typed low → the model mostly declined to type at all. That is not a hierarchy
//     failure; the taxonomy simply never engaged, and no amount of prompt wording about
//     specificity will move it.
//
// Every transcript here is written so its durable statement is unmistakably ABOUT the
// event — "last Tuesday's checkout outage ran forty minutes", not "the checkout service
// went down". The first phrasing was tried and the model typed it `object:checkout
// service`, which is correct for what that sentence is about, and told us nothing about
// which rung of the ladder it would pick.

// HierarchyOntologyTerms is the -ontology-terms value these cases are scored against.
//
// Exported so the invocation and the fixture cannot drift apart: a case expecting
// `outage` is meaningless unless `outage` is in the prompt, and the two living in
// different files is exactly how that stops being true.
const HierarchyOntologyTerms = "event,event/incident,event/incident/outage,person"

// ExtractionHierarchyFixture returns the specificity corpus.
func ExtractionHierarchyFixture() ExtractionCorpus {
	return ExtractionCorpus{Cases: []ExtractionCase{
		// The canary is the harness's own self-check and every corpus needs exactly
		// one — a corpus whose transcripts never reached the model must fail as a
		// harness fault, not as a model that types badly.
		ExtractionCanary(),

		// ---- the subtype is right, and available ----
		{
			Name:    "specific-subtype-when-it-fits",
			Ability: AbilitySpecificity,
			Turns: []string{
				"Last Tuesday's checkout outage ran about forty minutes. A bad config " +
					"push caused it and a rollback ended it.",
				"That sounds rough.",
			},
			Want: []ExpectedFact{{
				Why: "an outage is available in the ontology and the transcript describes one — " +
					"typing this `event` throws away the classification the operator built",
				Type:  "outage",
				AnyOf: []string{"checkout", "down", "outage"},
			}},
		},

		// ---- the MIDDLE of the ladder is right ----
		//
		// A separate case from the one above because the two failures are different:
		// this one catches a model that has learned "always take the deepest type".
		{
			Name:    "middle-of-the-ladder",
			Ability: AbilitySpecificity,
			Turns: []string{
				"Last month's avatar corruption incident took a day to clean up. " +
					"Nothing was ever unavailable — a deploy just mangled the files.",
				"Understood.",
			},
			Want: []ExpectedFact{{
				Why: "an incident that explicitly was NOT an outage — `incident` is the most " +
					"specific type the transcript supports, and `outage` would be invented precision",
				Type:  "incident",
				AnyOf: []string{"avatar", "deploy", "corrupt"},
			}},
		},

		// ---- the PARENT is right: the over-specification trap ----
		{
			Name:    "general-when-no-subtype-fits",
			Ability: AbilitySpecificity,
			Turns: []string{
				"My daughter's birthday party is on the 14th, we're doing it at the bowling alley.",
				"Sounds fun.",
			},
			Want: []ExpectedFact{{
				Why: "a birthday party is an event and nothing more specific applies — typing it " +
					"`incident` or `outage` invents a claim the transcript never made",
				Type:  "event",
				AnyOf: []string{"birthday", "party"},
			}},
		},

		// ---- a ROOT outside the ladder, unaffected by it ----
		//
		// Present because a hierarchy must not distort typing elsewhere: a model handed a
		// ladder sometimes starts reading everything as a rung.
		{
			Name:    "unrelated-root-is-untouched",
			Ability: AbilitySpecificity,
			Turns: []string{
				"My colleague Ada runs the platform team — she's the one to ask about deploys.",
				"Noted.",
			},
			Want: []ExpectedFact{{
				Why:   "a person is a person; the event ladder must not pull unrelated facts into it",
				Type:  "person",
				AllOf: []string{"ada"},
			}},
		},
	}}
}

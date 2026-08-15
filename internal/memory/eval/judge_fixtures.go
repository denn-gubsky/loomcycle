package eval

// judge_fixtures.go — the corpus that measures what the JUDGE refuses.
//
// A judge is a classifier in the write path, and its two error directions are not
// symmetric. A FALSE REFUSAL withholds a true fact, and the loss is invisible: nobody
// notices a fact that is not there. A FALSE ADMISSION keeps a fabrication, which leaves
// the store exactly where it is today. So the gate is on false refusals, and the
// fabrication cases are measured but do not block.
//
// BOTH DIRECTIONS ARE IN ONE CORPUS, and that is not symmetry for its own sake: a judge
// measured only on fabrications scores perfectly by refusing everything. That trap has
// already been walked into once in this line, by a corpus that measured subtype selection
// in a single direction and so could not tell a careful model from a lazy one.
//
// THE CASES ARE THE ONES THAT HAPPENED. The fabrications here are not invented to be
// catchable — they are the shapes a live model actually produced: specifics (a duration, a
// user count, a cause) attached to a sentence that mentions the right subject and states
// none of them, and a real quote sitting under a claim it has nothing to do with. That is
// deliberate, because a fabrication written to be obvious measures nothing.
//
// WHAT IS NOT IN HERE. No case turns on the difference between "unsupported" and a claim
// whose date or number the quote merely fails to mention — the shipped prompt tells the
// judge to call that unsupported, so a fixture expecting `unclear` for it would be
// scoring the model against the opposite of its instructions. When that reading changes,
// the prompt and this file move together.

// JudgeCorpusTerms is the -ontology-terms value the mistyping cases are scored against.
//
// Exported for the same reason the hierarchy corpus exports its own: a case expecting
// `mistyped` is meaningless unless the judge was shown the type list it is supposed to
// check against, and the two living in separate files is how that stops being true.
const JudgeCorpusTerms = "person,location,event,object,organization"

// JudgeFixture returns the shipped judge corpus.
func JudgeFixture() JudgeCorpus {
	return JudgeCorpus{Cases: []JudgeCase{
		// ---- the canary -------------------------------------------------------
		//
		// Every corpus needs exactly one, and this is the judge's: the claim IS its
		// quote, word for word. Any model that received the candidates at all
		// answers `supported`. If this case comes back refused, unclear or empty,
		// then the run's other numbers describe a model that was never asked the
		// question — and a "the judge refuses everything" report would otherwise be
		// the confident, wrong diagnosis.
		{
			Name:    "canary-the-claim-is-the-quote",
			Ability: AbilityEntailment,
			Canary:  true,
			Claim:   "The user lives in Cluj-Napoca.",
			Quote:   "The user lives in Cluj-Napoca.",
			Want:    VerdictSupported,
			Why:     "the claim and the quote are the same sentence",
		},

		// ---- must ADMIT: this is the number that gates ------------------------
		{
			Name:    "stated-plainly",
			Ability: AbilityEntailment,
			Claim:   "The user lives in Cluj-Napoca.",
			Quote:   "I live in Cluj-Napoca and I work on loomcycle every day.",
			Want:    VerdictSupported,
			Why:     "the quote states it outright; refusing this withholds a true fact",
		},
		{
			Name:    "paraphrased-not-quoted",
			Ability: AbilityEntailment,
			Claim:   "The user prefers boring, minimal solutions.",
			Quote:   "honestly I'd rather have the dull option that works than something clever",
			Want:    VerdictSupported,
			Why: "a fact PARAPHRASES its source by design — a judge that only accepts " +
				"near-quotation refuses most of the store",
		},
		{
			Name:    "the-quote-says-more-than-the-claim",
			Ability: AbilityEntailment,
			Claim:   "The checkout service went down yesterday.",
			Quote:   "the checkout service went down yesterday, we rolled back and it came back at 14:20",
			Want:    VerdictSupported,
			Why: "a quote may carry MORE than the claim; only the reverse is a problem, " +
				"and confusing the two directions is the likeliest false-refusal mode",
		},

		// ---- must REFUSE: the fabrications that actually happened -------------
		{
			Name:    "invented-specifics",
			Ability: AbilityFabrication,
			Claim:   "The checkout service went down yesterday for forty minutes, affecting 3148 users.",
			Quote:   "the checkout service went down yesterday, we rolled back",
			Want:    VerdictUnsupported,
			Why: "the duration and the user count are in no source — the observed failure, " +
				"and fluent enough that word overlap accepts it",
		},
		{
			Name:    "invented-cause",
			Ability: AbilityFabrication,
			Claim:   "The outage was caused by a bad config push.",
			Quote:   "the checkout service went down yesterday, we rolled back",
			Want:    VerdictUnsupported,
			Why:     "a rollback does not name a cause; the cause was supplied by the writer",
		},
		{
			Name:    "a-real-quote-under-an-unrelated-claim",
			Ability: AbilityFabrication,
			Claim:   "The user's postgres role needs CREATEROLE.",
			Quote:   "I live in Cluj-Napoca and I work on loomcycle every day.",
			Want:    VerdictUnsupported,
			Why: "the quote is genuine and has nothing to do with the claim — the case a " +
				"span check cannot catch, because the span is real",
		},

		// ---- must say UNCLEAR: the third verdict has to be reachable ----------
		//
		// Present because a judge with two usable verdicts guesses on everything in
		// between, and every guess it makes toward `unsupported` is a false refusal.
		{
			Name:    "half-of-a-compound-claim",
			Ability: AbilityPartial,
			Claim:   "The user works on loomcycle and deploys it on TrueNAS.",
			Quote:   "I work on loomcycle every day.",
			Want:    VerdictUnclear,
			Why:     "the quote carries the first half and says nothing about the second",
		},
		{
			Name:    "the-subject-is-a-pronoun",
			Ability: AbilityPartial,
			Claim:   "Ada runs the platform team.",
			Quote:   "she runs the platform team, ask her about deploys",
			Want:    VerdictUnclear,
			Why:     "the quote supports the claim about someone, but does not say it is Ada",
		},

		// ---- must say MISTYPED: true, and filed as the wrong kind of thing ----
		{
			Name:    "a-person-filed-as-a-location",
			Ability: AbilityMistyping,
			Claim:   "The user lives in Cluj-Napoca.",
			Quote:   "I live in Cluj-Napoca and I work on loomcycle every day.",
			Type:    "location",
			Subject: "user",
			Want:    VerdictMistyped,
			Why: "the quote does support the claim, and `user` is not a location — the " +
				"observed filing error, whose fix is a retype and not a deletion",
		},
		{
			Name:    "correctly-filed-must-not-be-called-mistyped",
			Ability: AbilityMistyping,
			Claim:   "The user lives in Cluj-Napoca.",
			Quote:   "I live in Cluj-Napoca and I work on loomcycle every day.",
			Type:    "location",
			Subject: "Cluj-Napoca",
			Want:    VerdictSupported,
			Why: "the other direction: a correctly filed fact must come back supported, or " +
				"the mistyping check becomes a second way to devalue clean facts",
		},
	}}
}

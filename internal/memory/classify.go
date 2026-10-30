package memory

import "context"

// The contract between Document op=classify and the model that settles what its rules
// and neighbourhood vote could not (memory.classifier). It lives here, beside the
// ontology types, so the Document tool depends on the contract and not on how a model
// is called.

// ClassifyOption is one class or concept the model may name, with what it means.
type ClassifyOption struct {
	Name    string
	Meaning string
}

// ClassifyRequest is the document as classify read it, and what may be said of it.
// Classes and Concepts are the only answers a model may give: a verdict naming
// anything else is dropped.
type ClassifyRequest struct {
	Title    string
	Text     string
	Classes  []ClassifyOption
	Concepts []ClassifyOption
	// ConceptsOnly asks for the concepts alone: the document's class is already
	// decided, and Classes is ignored.
	ConceptsOnly bool
}

// Classify verdict methods.
const (
	ClassifyMethodDecision = "decision"
	ClassifyMethodChat     = "chat"
)

// ClassifyVerdict is the model's answer.
type ClassifyVerdict struct {
	// Method is which model answered: decision or chat; "" when none did.
	Method string
	// Class is the class named, "" for none (or no usable answer).
	Class string
	// Probability is the decision model's probability for Class, and Margin its lead
	// over the runner-up; 0 for a chat verdict.
	Probability float64
	Margin      float64
	// Quote is the chat model's evidence, checked to occur in the document.
	Quote string
	// Concepts are the concepts the model says the document is about.
	Concepts []string
	// Reason says why there is no class, or why the decision answer was set aside.
	Reason string
}

// DocumentClassifier is memory.classifier as classify sees it. Classify never fails:
// every fault is a verdict with no class and a Reason.
type DocumentClassifier interface {
	Classify(ctx context.Context, req ClassifyRequest) ClassifyVerdict
}

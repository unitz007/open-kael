// Package rag defines the retrieval half of retrieval-augmented
// generation: how an agent's tools pull relevant context (codebase
// snippets, docs, whatever's been indexed) into a run without that context
// having to already be in the transcript. No embedding model or vector
// store lives here — this package only defines the shape a concrete
// implementation has to satisfy, the same role identity / identity.go plays for
// external-system credentials.
package rag

import "context"

// Document is one retrievable unit — a chunk of a file, a whole file, a
// doc snippet, whatever granularity an Indexer chose to store it at.
type Document struct {
	ID       string // unique within the source it came from
	Source   string // e.g. a file path or URL the content came from
	Content  string
	Metadata map[string]string // implementation-defined, e.g. {"language": "go", "start_line": "42"}
}

// Result is one match from a Query, ranked by relevance.
type Result struct {
	Document Document
	Score    float64 // similarity score — higher is more relevant; scale is implementation-defined
}

// Retriever is how an agent's tools query into an already-indexed source.
// Deliberately read-only and provider-agnostic — no embedding model or
// vector-store specifics leak into this interface, so a local file-backed
// implementation and a hosted vector DB can both satisfy it identically
// from a tool's perspective.
type Retriever interface {
	// Query returns up to topK documents most relevant to query, ranked
	// highest-score first.
	Query(ctx context.Context, query string, topK int) ([]Result, error)
}

// Indexer is how content gets into a Retriever's backing store in the
// first place. Kept separate from Retriever rather than folded in (the way
// memory.Memory combines read and write) because the two have a different
// trust boundary: almost anything that can query an index should be able
// to, but only a controlled ingestion path — not an arbitrary LLM tool
// call — should usually be able to add or remove what's in it.
type Indexer interface {
	// Index adds or updates documents in the store, keyed by Document.ID.
	Index(ctx context.Context, docs []Document) error
	// Delete removes documents by ID. Deleting an ID that isn't present is
	// not an error.
	Delete(ctx context.Context, ids []string) error
}

// ctxKey threads an agent's registered retrievers through ctx. Same
// reasoning as identity.WithIdentities: a tool can be built completely
// independently of any specific *Agent (a workflow's own tools are
// constructed before the owning agent even exists), so there's no *Agent
// reference to close over at construction time. Threading retrievers
// through ctx instead means they're resolved only once a specific agent's
// loop is actually running, by whichever tool needs them, regardless of
// where that tool was originally built.
type ctxKey struct{}

// WithRetrievers attaches a set of retrievers (keyed by name — an agent
// may have more than one indexed source, e.g. "codebase" and "docs") to
// ctx.
func WithRetrievers(ctx context.Context, retrievers map[string]Retriever) context.Context {
	return context.WithValue(ctx, ctxKey{}, retrievers)
}

// FromContext looks up a registered retriever by name from whichever
// agent's loop is currently executing. ok is false if ctx carries no
// retrievers at all, or none registered under that name.
func FromContext(ctx context.Context, name string) (Retriever, bool) {
	retrievers, _ := ctx.Value(ctxKey{}).(map[string]Retriever)
	r, ok := retrievers[name]
	return r, ok
}

package memory

// CoreBlockKeyPrefix is the reserved KV Memory namespace for RFC BL P1 core
// memory blocks: a block labeled <label> is stored at `core/<label>`. Single
// source of truth shared by the Memory tool (write enforcement) and the HTTP
// injection reader.
const CoreBlockKeyPrefix = "core/"

// DocumentChunkKeyPrefix is the reserved KV Memory namespace for RFC AK Document
// chunk BODIES: a chunk's body lives at `doc.chunk:<uuid>` in the same
// (tenant, scope, scope_id) partition as an agent's own memory.
//
// That sharing is what makes this a single source of truth rather than a detail.
// Chunk bodies are Document storage — written by the Document tool, governed by
// its own quota — but they sit in the memory keyspace, so anything reasoning about
// "how much memory does this agent have" MUST exclude them. A user with a few
// thousand document chunks is a normal, intended state (a chunked-graph store
// scaling to thousands of chunks is the point of the feature), and counting those
// against the per-scope memory quota locks the agent out of writing memory at all.
const DocumentChunkKeyPrefix = "doc.chunk:"

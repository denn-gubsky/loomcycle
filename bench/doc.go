// Package bench is the loomcycle model-capability benchmarking harness.
//
// The harness drives an externally-running loomcycle instance over its
// HTTP MCP transport (POST /v1/_mcp), registering one dynamic agent per
// (model, tier) candidate and exercising it against a fixed battery of
// test cases. Each case is graded on three independent axes —
// structural (output shape), functional (tool-call sequence), and
// semantic (judge-model rating). The aggregate matrix tells the
// operator which third-party models earn a slot in the production
// user_tier overlay for jobs-search-agent.
//
// See bench/README.md for run instructions; bench/cmd/lc-bench is the
// entry point.
package bench

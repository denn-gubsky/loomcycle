// ats-scraper — RFC J worked example: a nightly ATS scrape as a code-agent.
//
// Fetches job listings across four boards with the built-in WebFetch tool,
// dedupes against per-user memory, and hands fresh jobs to the consumer's own
// MCP tool (mcp__jobs__ingestJobs). ZERO LLM calls — pure deterministic glue.
// The scheduler fires it like any other agent (see this directory's README.md).
//
// Every tool here is real: WebFetch is a loomcycle built-in;
// mcp__jobs__ingestJobs is exposed by jobs-search-agent's own /api/mcp route
// (the loomcycle MCP-server integration pattern). All are gated by the agent's
// allowed_tools and dispatched by the loop — WebFetch's host allowlist and the
// MCP server's ${run.credentials.jobs} bearer apply unchanged.
//
// Tool calls are written synchronously (const x = WebFetch(...)); the loop
// dispatches each and the result returns inline — no await, no callbacks.

function run(input) {
  var boards = ["greenhouse", "lever", "workable", "ashby"];
  var seen = Memory.get({ scope: "user", key: "ats_seen_ids" }) || {};
  var fresh = [];

  for (var i = 0; i < boards.length; i++) {
    var board = boards[i];
    // Built-in WebFetch GETs the URL and returns the text body. The host must
    // be in the agent's allowed_hosts (operator policy), same as for an LLM.
    var body = WebFetch({ url: "https://" + board + ".example/api/jobs" });
    var jobs = parseJobs(body); // deterministic parse, no LLM
    for (var j = 0; j < jobs.length; j++) {
      var job = jobs[j];
      if (!seen[job.id]) {
        fresh.push(job);
        seen[job.id] = Date.now();
      }
    }
  }

  Memory.set({ scope: "user", key: "ats_seen_ids", value: seen });

  if (fresh.length > 0) {
    // jobs-search-agent's MCP tool ingests the fresh jobs for this user. The
    // bearer is substituted at the MCP transport (${run.credentials.jobs}) —
    // this code never sees the token.
    mcp__jobs__ingestJobs({ user_id: input.metadata.user_id, jobs: fresh });
  }

  return { final_text: "ingested " + fresh.length + " fresh jobs across " + boards.length + " boards" };
}

// parseJobs deterministically extracts job objects from a fetched body. This
// stub assumes a JSON `{ "jobs": [{id,...}] }` shape; a real parser would
// handle each board's HTML/JSON. No LLM, no fetch — pure string work.
function parseJobs(body) {
  try {
    var data = JSON.parse(body);
    return data && data.jobs ? data.jobs : [];
  } catch (e) {
    return [];
  }
}

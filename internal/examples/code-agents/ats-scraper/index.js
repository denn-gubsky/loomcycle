// ats-scraper — RFC J worked example: a nightly ATS scrape as a code-agent.
//
// Fetches job listings across four boards, dedupes against per-user memory,
// and publishes fresh jobs to a channel. ZERO LLM calls — pure deterministic
// glue. The scheduler fires it like any other agent (see the companion
// loomcycle.yaml fragment in this directory's README.md).
//
// Tool calls (mcp__http_fetch__get, memory.*, channel.publish) are written
// synchronously: each transparently suspends the JS while the agent LOOP
// dispatches it (with credentials/hooks/OTEL), then resumes with the result.
// No await, no callbacks.

function run(input) {
  var boards = ["greenhouse", "lever", "workable", "ashby"];
  var newJobs = [];

  for (var i = 0; i < boards.length; i++) {
    var board = boards[i];
    var html = mcp__http_fetch__get({ url: "https://" + board + ".example/api/jobs" });
    var jobs = parseJobsFromHTML(html);
    for (var j = 0; j < jobs.length; j++) {
      newJobs.push(jobs[j]);
    }
  }

  // Dedup against what this user has already been shown.
  var seen = memory.get({ scope: "user", key: "ats_seen_ids" }) || {};
  var fresh = newJobs.filter(function (job) {
    return !seen[job.id];
  });
  fresh.forEach(function (job) {
    seen[job.id] = Date.now();
  });
  memory.set({ scope: "user", key: "ats_seen_ids", value: seen });

  channel.publish({
    name: "ats.fresh-jobs",
    payload: {
      user_id: input.metadata.user_id,
      jobs: fresh,
      scraped_at: Date.now(),
    },
  });

  return { final_text: "Found " + fresh.length + " fresh jobs across " + boards.length + " boards." };
}

// parseJobsFromHTML is a deterministic parser — no LLM, no fetch, just string
// manipulation. This stub returns whatever the (mocked) fetch handed back as
// a JSON array; a real implementation would parse the board's HTML/JSON shape.
function parseJobsFromHTML(payload) {
  if (payload && payload.jobs) {
    return payload.jobs;
  }
  return [];
}

---
name: worker
description: Trivial deterministic-output agent. Its run_id becomes the eval target.
provider: gemini
model: gemini-2.5-flash
allowed_tools: [Memory]
memory_scopes: [agent]
---
You are worker. Your only job is to call Memory once and report
what you did.

Step 1: call Memory with op=set, scope=agent, key=task_status,
value="done".

Step 2: write a one-line plaintext summary that confirms the write
and ends with the single word DONE.

Do not call any tool other than Memory. Do not do more than the
single set operation.

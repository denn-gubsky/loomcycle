# @loomcycle/client

Minimal TypeScript client for the [loomcycle](https://github.com/denn-gubsky/loomcycle) sidecar.

```ts
import { LoomcycleClient } from "@loomcycle/client";

const client = new LoomcycleClient({
  baseUrl: "http://127.0.0.1:8787",
  authToken: process.env.LOOMCYCLE_AUTH_TOKEN,
});

for await (const ev of client.runStreaming({
  agent: "default",
  segments: [{ role: "user", content: [{ type: "trusted-text", text: "Hello" }] }],
})) {
  if (ev.type === "text") process.stdout.write(ev.text ?? "");
}
```

Apache-2.0.

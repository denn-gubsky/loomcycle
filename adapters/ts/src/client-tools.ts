// Client-tool host (RFC BC) — the client side of client-executed tools. Open a
// persistent WebSocket to loomcycle, register the tools you can run on the
// user's machine (browser DOM, local FS, shell), and answer the `invoke` frames
// loomcycle routes to you when an agent of your principal calls one. loomcycle
// returns your reply to the agent as an ordinary tool result — the agent follows
// no protocol.
//
// Transport note: the adapter is otherwise fetch/SSE-only and takes no WebSocket
// dependency. connectClientTools uses the global `WebSocket` (browsers, Node
// 22+); on older Node pass `WebSocketImpl` (e.g. the `ws` package). The bearer
// rides the `Sec-WebSocket-Protocol` subprotocol (browsers can't set an
// Authorization header on a WebSocket).

/** The app-level subprotocol the server negotiates for /v1/client-tools. */
export const CLIENT_TOOL_SUBPROTOCOL = "loomcycle.client-tools.v1";

/** A tool the client offers to run locally (advertised in the hello frame). */
export interface ClientToolSchema {
  name: string; // bare name, e.g. "browser.read_page" (agents call client:browser.read_page)
  description?: string;
  /** JSON Schema for the tool input (object). */
  input_schema?: unknown;
}

/** An inbound tool call to execute on the user's machine. */
export interface ClientToolInvocation {
  tool: string; // bare tool name
  input: unknown;
  callId: string;
  runId?: string;
  agentId?: string;
}

/** A minimal structural WebSocket type so we don't depend on lib.dom or `ws`. */
export interface WebSocketLike {
  send(data: string): void;
  close(code?: number, reason?: string): void;
  onopen: ((ev: unknown) => void) | null;
  onclose: ((ev: unknown) => void) | null;
  onerror: ((ev: unknown) => void) | null;
  onmessage: ((ev: { data: unknown }) => void) | null;
}

export type WebSocketCtor = new (url: string, protocols?: string | string[]) => WebSocketLike;

export interface ConnectClientToolsOptions {
  /** The tools this client provides. */
  tools: ClientToolSchema[];
  /**
   * Handler for an inbound invoke. Return the tool's output (any JSON value); a
   * thrown error is reported to the agent as a tool error. Confirm mutating /
   * destructive actions with the user before executing — loomcycle cannot.
   */
  onInvoke: (inv: ClientToolInvocation) => unknown | Promise<unknown>;
  /** WebSocket implementation. Defaults to the global; pass `ws` on Node < 22. */
  WebSocketImpl?: WebSocketCtor;
  /** Auto-reconnect on drop (default true). */
  reconnect?: boolean;
  /** Reconnect backoff in ms (default 2000). */
  reconnectDelayMs?: number;
  /** Called on lifecycle transitions. */
  onStatus?: (status: "connecting" | "open" | "closed") => void;
  /** Called on a transport error (does not stop the host unless you close it). */
  onError?: (err: unknown) => void;
}

/**
 * ClientToolHost owns the WebSocket lifecycle: connect → hello → dispatch each
 * invoke to onInvoke → reply with a result, with auto-reconnect. Protocol
 * ping/pong is handled by the WebSocket layer, so there is no app heartbeat.
 * Construct via LoomcycleClient.connectClientTools; call close() to stop.
 */
export class ClientToolHost {
  private ws?: WebSocketLike;
  private closed = false;

  constructor(
    private readonly url: string,
    private readonly authToken: string | undefined,
    private readonly opts: ConnectClientToolsOptions,
  ) {}

  /** Open the connection (called for you by connectClientTools). */
  start(): void {
    this.connect();
  }

  /** Stop the host + close the socket; suppresses reconnect. */
  close(): void {
    this.closed = true;
    this.ws?.close(1000, "client closed");
  }

  private connect(): void {
    if (this.closed) return;
    const WS: WebSocketCtor | undefined =
      this.opts.WebSocketImpl ?? (globalThis as { WebSocket?: WebSocketCtor }).WebSocket;
    if (!WS) {
      throw new Error(
        "connectClientTools: no WebSocket implementation available — pass WebSocketImpl (e.g. the `ws` package) on Node < 22",
      );
    }
    const protocols = [CLIENT_TOOL_SUBPROTOCOL];
    if (this.authToken) protocols.push("bearer." + this.authToken);

    this.opts.onStatus?.("connecting");
    const ws = new WS(this.url, protocols);
    this.ws = ws;

    ws.onopen = () => {
      this.opts.onStatus?.("open");
      this.sendJSON({ type: "hello", client: "@loomcycle/client", tools: this.opts.tools });
    };
    ws.onerror = (ev) => this.opts.onError?.(ev);
    ws.onclose = () => {
      this.opts.onStatus?.("closed");
      this.scheduleReconnect();
    };
    ws.onmessage = (ev) => void this.onMessage(ev.data);
  }

  private async onMessage(data: unknown): Promise<void> {
    let frame: Record<string, unknown>;
    try {
      const text = typeof data === "string" ? data : String(data);
      frame = JSON.parse(text) as Record<string, unknown>;
    } catch {
      return; // ignore unparseable frames
    }
    if (frame.type !== "invoke") return; // hello_ok / anything else — ignore
    const callId = String(frame.call_id ?? "");
    try {
      const output = await this.opts.onInvoke({
        tool: String(frame.tool ?? ""),
        input: frame.input,
        callId,
        runId: frame.run_id as string | undefined,
        agentId: frame.agent_id as string | undefined,
      });
      this.sendJSON({ type: "result", call_id: callId, ok: true, output });
    } catch (e) {
      this.sendJSON({
        type: "result",
        call_id: callId,
        ok: false,
        error: e instanceof Error ? e.message : String(e),
      });
    }
  }

  private sendJSON(v: unknown): void {
    try {
      this.ws?.send(JSON.stringify(v));
    } catch (e) {
      this.opts.onError?.(e);
    }
  }

  private scheduleReconnect(): void {
    if (this.closed || this.opts.reconnect === false) return;
    const delay = this.opts.reconnectDelayMs ?? 2000;
    setTimeout(() => this.connect(), delay);
  }
}

/** Build the ws(s):// URL for the client-tool endpoint from an http(s) base. */
export function clientToolsURL(baseUrl: string): string {
  return baseUrl.replace(/\/$/, "").replace(/^http/, "ws") + "/v1/client-tools";
}

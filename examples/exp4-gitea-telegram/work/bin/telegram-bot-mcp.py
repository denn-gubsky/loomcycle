#!/usr/bin/env python3
"""Minimal Telegram BOT MCP server (stdio, JSON-RPC line-delimited).

Exposes one tool, `send_message`, that POSTs to the Telegram Bot API. Secrets
come from the environment (never args/argv): LOOMCYCLE_TELEGRAM_BOT_TOKEN and a
default TELEGRAM_CHAT_ID. stdlib-only, no external deps.
"""
import json, os, sys, urllib.request, urllib.parse

TOKEN_ENV = "LOOMCYCLE_TELEGRAM_BOT_TOKEN"
CHAT_ENV = "TELEGRAM_CHAT_ID"

TOOLS = [{
    "name": "send_message",
    "description": "Send a text message to Telegram via the bot. Uses the default chat "
                   "(TELEGRAM_CHAT_ID) unless chat_id is given. Supports parse_mode (HTML/Markdown).",
    "inputSchema": {
        "type": "object",
        "required": ["text"],
        "properties": {
            "text": {"type": "string", "description": "Message body."},
            "chat_id": {"type": "string", "description": "Override target chat id (default: env TELEGRAM_CHAT_ID)."},
            "parse_mode": {"type": "string", "enum": ["HTML", "Markdown", "MarkdownV2"], "description": "Optional formatting."},
        },
    },
}]


def send_message(args):
    token = os.environ.get(TOKEN_ENV, "")
    if not token:
        raise RuntimeError(f"{TOKEN_ENV} not set in the MCP server environment")
    chat_id = args.get("chat_id") or os.environ.get(CHAT_ENV, "")
    if not chat_id:
        raise RuntimeError(f"no chat_id (arg) and {CHAT_ENV} not set")
    payload = {"chat_id": chat_id, "text": args["text"]}
    if args.get("parse_mode"):
        payload["parse_mode"] = args["parse_mode"]
    data = urllib.parse.urlencode(payload).encode()
    url = f"https://api.telegram.org/bot{token}/sendMessage"
    req = urllib.request.Request(url, data=data, method="POST")
    with urllib.request.urlopen(req, timeout=15) as r:
        body = json.loads(r.read().decode())
    # never echo the token; return only the Telegram result envelope
    return {"ok": body.get("ok"), "message_id": (body.get("result") or {}).get("message_id"), "chat_id": chat_id}


def reply(rid, result=None, error=None):
    msg = {"jsonrpc": "2.0", "id": rid}
    if error is not None:
        msg["error"] = error
    else:
        msg["result"] = result
    sys.stdout.write(json.dumps(msg) + "\n")
    sys.stdout.flush()


def main():
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
        except Exception:
            continue
        method, rid = req.get("method"), req.get("id")
        if method == "initialize":
            reply(rid, {
                "protocolVersion": req.get("params", {}).get("protocolVersion", "2024-11-05"),
                "capabilities": {"tools": {}},
                "serverInfo": {"name": "telegram-bot-mcp", "version": "1.0"},
            })
        elif method == "notifications/initialized":
            continue
        elif method == "tools/list":
            reply(rid, {"tools": TOOLS})
        elif method == "tools/call":
            params = req.get("params", {})
            name, args = params.get("name"), params.get("arguments", {})
            try:
                if name != "send_message":
                    raise RuntimeError(f"unknown tool: {name}")
                out = send_message(args)
                reply(rid, {"content": [{"type": "text", "text": json.dumps(out)}], "isError": False})
            except Exception as e:
                reply(rid, {"content": [{"type": "text", "text": f"send_message failed: {e}"}], "isError": True})
        elif rid is not None:
            reply(rid, error={"code": -32601, "message": f"method not found: {method}"})


if __name__ == "__main__":
    main()

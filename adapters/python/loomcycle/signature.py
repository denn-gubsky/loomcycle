"""Verifying a signed loomcycle hook delivery.

A HookDef whose webhook body names a ``signing_secret`` signs every call::

    X-Loomcycle-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256>

The MAC is keyed by the secret and computed over ``<t>.<raw body>``.
``X-Loomcycle-Hook`` names the hook and ``X-Loomcycle-Delivery`` identifies the
delivery (a retry keeps it), so a receiver can drop one it already handled.

Standard library only (hmac, hashlib): a receiver is an HTTP endpoint and
needs nothing of the gRPC client to verify a call.
"""

from __future__ import annotations

import hashlib
import hmac
import re
import time
from typing import Optional, Union

__all__ = ["verify_loomcycle_signature", "DEFAULT_TOLERANCE_SEC"]

DEFAULT_TOLERANCE_SEC = 300
"""The window loomcycle's own receiver uses, in seconds, either way."""

_UNIX_RE = re.compile(r"[+-]?[0-9]+")
_HEX_RE = re.compile(r"(?:[0-9a-fA-F]{2})+")


def verify_loomcycle_signature(
    raw_body: Union[bytes, str],
    header: str,
    secret: Union[bytes, str],
    *,
    tolerance_sec: float = DEFAULT_TOLERANCE_SEC,
    now: Optional[float] = None,
) -> bool:
    """Return whether ``header`` signs ``raw_body`` under ``secret``, within
    ``tolerance_sec`` of ``now`` (unix seconds; default ``time.time()``).

    ``raw_body`` must be the request body exactly as received, before any JSON
    parsing; a ``str`` is hashed as its UTF-8 encoding. Fails closed: a
    malformed header, a non-integer or out-of-window ``t``, non-hex ``v1`` or a
    wrong MAC all return ``False``. The MAC is compared with
    ``hmac.compare_digest`` (constant time).
    """
    if not isinstance(header, str) or not secret:
        return False
    ts = ""
    mac_hex = ""
    for part in header.split(","):
        part = part.strip()
        if part.startswith("t="):
            ts = part[2:]
        elif part.startswith("v1="):
            mac_hex = part[3:]
    if not _UNIX_RE.fullmatch(ts) or not _HEX_RE.fullmatch(mac_hex):
        return False
    current = time.time() if now is None else now
    if not tolerance_sec >= 0 or abs(current - int(ts)) > tolerance_sec:
        return False
    body = raw_body.encode("utf-8") if isinstance(raw_body, str) else bytes(raw_body)
    key = secret.encode("utf-8") if isinstance(secret, str) else bytes(secret)
    # The MAC covers t exactly as written in the header, then ".", then the body.
    expected = hmac.new(key, ts.encode("ascii") + b"." + body, hashlib.sha256).digest()
    return hmac.compare_digest(expected, bytes.fromhex(mac_hex))

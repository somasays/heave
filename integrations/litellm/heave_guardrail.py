"""heave <-> LiteLLM integration: a CustomGuardrail that enforces heave's spend &
quota firewall OUT-OF-BAND, with heave NEVER in the data path.

How it works (see docs/adr/0004 and 0007): LiteLLM's guardrail lifecycle already
fires around every LLM call. This adapter maps those firings onto heave's
reserve/settle/release decision API:

    async_pre_call_hook        -> POST /v1/guard/reserve   (deny => raise => call blocked PRE-vendor)
    async_post_call_success_hook -> POST /v1/guard/settle  (reconcile estimate -> actual usage)
    async_post_call_failure_hook -> POST /v1/guard/release (the call never billed)

The reservation_id returned by reserve is threaded through `data["metadata"]` so
settle/release can find it. Only a scope + a number crosses to heave — never the
prompt or the response. The actual LLM traffic still flows LiteLLM -> vendor
directly, so heave is a decision point, not a proxy hop.

Install:  pip install httpx  (LiteLLM proxy is already present)
Wire it:  see example_config.yaml
"""

from __future__ import annotations

import hashlib
import os
from typing import Any

import httpx
from fastapi import HTTPException

try:  # LiteLLM proxy provides these; keep import-time failures legible.
    from litellm.integrations.custom_guardrail import CustomGuardrail
    from litellm.proxy._types import UserAPIKeyAuth
except Exception:  # pragma: no cover - only importable inside a LiteLLM proxy
    CustomGuardrail = object  # type: ignore
    UserAPIKeyAuth = Any  # type: ignore

_META = "metadata"
_RESV = "heave_reservation_id"


class HeaveGuardrail(CustomGuardrail):
    """Enforce heave budgets around every LiteLLM call.

    Params (guardrail litellm_params, or env fallback):
      heave_url         base URL of the heave gateway        (env HEAVE_URL)
      heave_admin_key   a heave ADMIN key — the PEP credential (env HEAVE_ADMIN_KEY)
      prices            optional {model: {input_per_mtok, output_per_mtok}} so the
                        adapter can estimate USD for $/min and per-run $ caps. If a
                        model has no price, usd=0 is sent (token/concurrency caps
                        still apply); heave-side pricing is a planned enhancement.
    """

    def __init__(self, heave_url: str | None = None, heave_admin_key: str | None = None,
                 prices: dict[str, dict[str, float]] | None = None, **kwargs: Any) -> None:
        self.base = (heave_url or os.environ["HEAVE_URL"]).rstrip("/")
        self.admin_key = heave_admin_key or os.environ["HEAVE_ADMIN_KEY"]
        self.prices = prices or {}
        self.http = httpx.AsyncClient(timeout=5.0)
        super().__init__(**kwargs)

    # --- scope + estimate mapping ---------------------------------------------

    def _scope(self, key: "UserAPIKeyAuth", data: dict) -> tuple[str, str]:
        """Map the LiteLLM caller to a heave (key_sha256, run_id).

        key_sha256 priority: an explicit `metadata.heave_key_sha256`, else the
        SHA-256 of the caller's virtual key (provision heave keys with THIS hash,
        or supply the mapping in metadata). run_id: `metadata.heave_run_id` or the
        `X-Heave-Run-Id` request header.
        """
        meta = (data.get(_META) or {})
        key_sha = meta.get("heave_key_sha256")
        if not key_sha:
            raw = getattr(key, "api_key", None) or getattr(key, "token", None) or ""
            key_sha = hashlib.sha256(raw.encode()).hexdigest() if raw else ""
        run_id = meta.get("heave_run_id") or _header(data, "x-heave-run-id") or ""
        return key_sha, run_id

    def _estimate(self, data: dict) -> dict:
        model = data.get("model", "")
        max_out = int(data.get("max_tokens") or data.get("max_completion_tokens") or 0)
        in_tokens = _count_input_tokens(model, data.get("messages") or [])
        tokens = in_tokens + max_out
        price = self.prices.get(model)
        usd = 0.0
        if price:
            usd = (in_tokens * price.get("input_per_mtok", 0.0)
                   + max_out * price.get("output_per_mtok", 0.0)) / 1_000_000.0
        return {"usd": round(usd, 8), "tokens": tokens}

    def _actual(self, data: dict, response: Any) -> dict:
        usage = getattr(response, "usage", None) or {}
        pt = _get(usage, "prompt_tokens")
        ct = _get(usage, "completion_tokens")
        tokens = int(_get(usage, "total_tokens") or (pt + ct))
        price = self.prices.get(data.get("model", ""))
        usd = 0.0
        if price:
            usd = (pt * price.get("input_per_mtok", 0.0)
                   + ct * price.get("output_per_mtok", 0.0)) / 1_000_000.0
        return {"usd": round(usd, 8), "tokens": tokens}

    # --- the three hooks -------------------------------------------------------

    async def async_pre_call_hook(self, user_api_key_dict: "UserAPIKeyAuth", cache: Any,
                                  data: dict, call_type: str):
        key_sha, run_id = self._scope(user_api_key_dict, data)
        body = {"key_sha256": key_sha, "run_id": run_id, "estimate": self._estimate(data)}
        r = await self.http.post(f"{self.base}/v1/guard/reserve", headers=self._auth(), json=body)
        r.raise_for_status()
        verdict = r.json()
        if not verdict.get("admitted"):
            # heave says stop: reject PRE-vendor. 429 (budget/velocity) or 403 (killed).
            node = verdict.get("binding_node") or ""
            raise HTTPException(
                status_code=verdict.get("http_status", 429),
                detail={"error": {"type": "heave_firewall",
                                   "message": f"blocked by heave: {verdict.get('reason')}"
                                              + (f" at {node}" if node else "")}})
        data.setdefault(_META, {})[_RESV] = verdict["reservation_id"]
        return data

    async def async_post_call_success_hook(self, data: dict,
                                           user_api_key_dict: "UserAPIKeyAuth", response: Any):
        rid = (data.get(_META) or {}).get(_RESV)
        if not rid:
            return response
        await self._reconcile("settle", {"reservation_id": rid, "actual": self._actual(data, response)})
        return response

    async def async_post_call_failure_hook(self, request_data: dict,
                                           original_exception: Exception,
                                           user_api_key_dict: "UserAPIKeyAuth"):
        rid = (request_data.get(_META) or {}).get(_RESV)
        if rid:
            await self._reconcile("release", {"reservation_id": rid})

    # --- helpers ---------------------------------------------------------------

    def _auth(self) -> dict:
        return {"Authorization": f"Bearer {self.admin_key}"}

    async def _reconcile(self, verb: str, body: dict) -> None:
        # A failed reconcile is non-fatal: heave's reservation lease self-heals the
        # hold, so never break the caller's response over a settle/release blip.
        try:
            await self.http.post(f"{self.base}/v1/guard/{verb}", headers=self._auth(), json=body)
        except Exception:
            pass


def _header(data: dict, name: str) -> str:
    headers = (data.get("proxy_server_request", {}) or {}).get("headers", {}) or {}
    return headers.get(name) or headers.get(name.title()) or ""


def _count_input_tokens(model: str, messages: list) -> int:
    try:
        from litellm import token_counter
        return int(token_counter(model=model, messages=messages))
    except Exception:
        # Fallback heuristic (chars/4) if the tokenizer isn't available.
        chars = sum(len(str(m.get("content", ""))) for m in messages)
        return max(1, chars // 4)


def _get(usage: Any, field: str) -> int:
    if isinstance(usage, dict):
        return int(usage.get(field) or 0)
    return int(getattr(usage, field, 0) or 0)

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

from . import SCHEMA_VERSION


class ProtocolError(ValueError):
    pass


def finite_number(value: Any, field: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise ProtocolError(f"{field} must be a number")
    out = float(value)
    if not math.isfinite(out):
        raise ProtocolError(f"{field} must be finite")
    return out


def positive_number(value: Any, field: str) -> float:
    out = finite_number(value, field)
    if out <= 0:
        raise ProtocolError(f"{field} must be > 0")
    return out


def require_list(value: Any, field: str) -> list[Any]:
    if not isinstance(value, list):
        raise ProtocolError(f"{field} must be an array")
    return value


def require_dict(value: Any, field: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise ProtocolError(f"{field} must be an object")
    return value


@dataclass(frozen=True)
class ParsedRequest:
    request_id: str
    payload: dict[str, Any]


def parse_request(raw: Any) -> ParsedRequest:
    payload = require_dict(raw, "request")
    version = payload.get("schema_version")
    if version != SCHEMA_VERSION:
        raise ProtocolError(
            f"unsupported schema_version {version!r}; expected {SCHEMA_VERSION}"
        )
    request_id = payload.get("request_id")
    if not isinstance(request_id, str) or not request_id:
        raise ProtocolError("request_id must be a non-empty string")
    slots = require_list(payload.get("slots"), "slots")
    if not slots:
        raise ProtocolError("slots must not be empty")
    require_list(payload.get("storages", []), "storages")
    require_list(payload.get("flex_loads", []), "flex_loads")
    require_list(payload.get("thermal_loads", []), "thermal_loads")
    require_dict(
        payload.get("commercial_constraints", {}),
        "commercial_constraints",
    )
    return ParsedRequest(request_id=request_id, payload=payload)


def error_response(request_id: str, code: str, message: str) -> dict[str, Any]:
    return {
        "schema_version": SCHEMA_VERSION,
        "request_id": request_id,
        "ok": False,
        "error": {"code": code, "message": message},
    }

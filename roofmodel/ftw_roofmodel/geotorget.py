"""Lantmaeteriet Geotorget access: authentication and STAC search.

Two products are used, both free open data (CC BY 4.0) but both gated behind a
Geotorget account the operator orders themselves:

  * *Byggnad Nedladdning, vektor* -- building footprint polygons.
  * *Laserdata Nedladdning, Skog* -- airborne LiDAR, 1-2 points/m2, from 2018.

Credentials are the operator's own and are never shipped, logged or echoed back
through the API. FTW stores them the same way it stores `weather.api_key`, and
redacts them in config responses.

Only `requests` is used. The STAC API is plain JSON over HTTP, so pulling in
pystac-client would add a dependency for a search body we can write in six
lines -- and a thinner surface is easier to keep working when Lantmaeteriet
moves an endpoint.
"""

from __future__ import annotations

import dataclasses
import datetime as dt
from typing import Any, Iterable

DEFAULT_BASE_URL = "https://api.lantmateriet.se"

# Collection ids as published in Lantmaeteriet's STAC catalogue.
COLLECTION_BUILDINGS = "byggnad-nedladdning-vektor"
COLLECTION_LIDAR = "laserdata-nedladdning-skog"


class GeotorgetError(RuntimeError):
    """Any failure talking to Geotorget."""


class MissingCredentials(GeotorgetError):
    """No usable credentials were supplied."""


@dataclasses.dataclass(frozen=True)
class Credentials:
    username: str
    password: str

    def validate(self) -> None:
        if not self.username or not self.password:
            raise MissingCredentials(
                "Geotorget username and token are both required; order access at "
                "https://geotorget.lantmateriet.se and set roofmodel.geotorget_username "
                "and roofmodel.geotorget_token"
            )


@dataclasses.dataclass
class StacItem:
    """One STAC item, reduced to what the pipeline needs."""

    item_id: str
    collection: str
    assets: dict[str, str]
    captured_at: dt.datetime | None
    raw: dict[str, Any] = dataclasses.field(default_factory=dict, repr=False)

    def asset_url(self, *preferred: str) -> str | None:
        """First matching asset href, trying each preferred key in order."""
        for key in preferred:
            if key in self.assets:
                return self.assets[key]
        return next(iter(self.assets.values()), None)


def _parse_datetime(value: str | None) -> dt.datetime | None:
    """Parse a STAC RFC 3339 timestamp.

    Lantmaeteriet is backfilling `properties.datetime` across 2026, so it is
    routinely absent. That is a missing provenance date, not an error -- the UI
    degrades to "capture date unknown" rather than refusing the model.
    """
    if not value:
        return None
    try:
        return dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None


def _item_from_feature(feature: dict[str, Any]) -> StacItem:
    props = feature.get("properties") or {}
    assets = {
        name: asset.get("href", "")
        for name, asset in (feature.get("assets") or {}).items()
        if asset.get("href")
    }
    captured = _parse_datetime(props.get("datetime")) or _parse_datetime(
        props.get("start_datetime")
    )
    if captured is None:
        # Laser strips carry an acquisition date as `datum` (e.g. "20180301")
        # even where the STAC datetime has not been backfilled yet.
        datum = props.get("datum")
        if datum:
            try:
                captured = dt.datetime.strptime(str(datum), "%Y%m%d").replace(
                    tzinfo=dt.timezone.utc
                )
            except ValueError:
                captured = None
    return StacItem(
        item_id=feature.get("id", ""),
        collection=feature.get("collection", ""),
        assets=assets,
        captured_at=captured,
        raw=feature,
    )


class GeotorgetClient:
    """Thin STAC client for Lantmaeteriet's download APIs."""

    def __init__(
        self,
        credentials: Credentials,
        session: Any = None,
        base_url: str = DEFAULT_BASE_URL,
        timeout: float = 60.0,
    ) -> None:
        credentials.validate()
        self._credentials = credentials
        self._base_url = base_url.rstrip("/")
        self._timeout = timeout
        if session is None:
            import requests  # imported lazily so tests can inject a fake session

            session = requests.Session()
            session.auth = (credentials.username, credentials.password)
        self._session = session

    def search(
        self,
        collection: str,
        bbox_sweref: tuple[float, float, float, float],
        limit: int = 20,
    ) -> list[StacItem]:
        """POST /stac/search for one collection over a SWEREF 99 TM bbox.

        bbox is (min_easting, min_northing, max_easting, max_northing); the
        catalogue is published in EPSG:3006, so no reprojection happens here.
        """
        body = {
            "collections": [collection],
            "bbox": list(bbox_sweref),
            "limit": limit,
        }
        url = f"{self._base_url}/stac/search"
        try:
            resp = self._session.post(url, json=body, timeout=self._timeout)
        except Exception as exc:  # network, DNS, TLS
            raise GeotorgetError(f"STAC search failed: {exc}") from exc
        if resp.status_code in (401, 403):
            raise MissingCredentials(
                f"Geotorget rejected the credentials for {collection} "
                f"(HTTP {resp.status_code}). Check the account has ordered access "
                "to this product."
            )
        if resp.status_code != 200:
            raise GeotorgetError(f"STAC search returned HTTP {resp.status_code}")
        payload = resp.json()
        return [_item_from_feature(f) for f in payload.get("features", [])]

    def download(self, url: str) -> bytes:
        """Fetch one asset."""
        try:
            resp = self._session.get(url, timeout=self._timeout)
        except Exception as exc:
            raise GeotorgetError(f"asset download failed: {exc}") from exc
        if resp.status_code != 200:
            raise GeotorgetError(f"asset download returned HTTP {resp.status_code}")
        return resp.content


def newest_capture(items: Iterable[StacItem]) -> dt.datetime | None:
    """Most recent known capture date across items, or None if none carry one."""
    dates = [i.captured_at for i in items if i.captured_at is not None]
    return max(dates) if dates else None

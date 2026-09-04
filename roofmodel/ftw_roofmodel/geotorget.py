"""STAC catalog access: authentication, search, and asset download.

The default catalog is Lantmaeteriet's Geotorget, where two products are used,
both free open data (CC BY 4.0) but both gated behind a Geotorget account the
operator orders themselves:

  * *Byggnad Nedladdning, vektor* -- building footprint polygons.
  * *Laserdata Nedladdning, Skog* -- airborne LiDAR, 1-2 points/m2, from 2018.

Authentication is HTTP Basic with the operator's own Geotorget account
username and password: Lantmaeteriet provides no OAuth for its STAC download
APIs, so the account credential is the only door. Credentials are the
operator's own and are never shipped, logged or echoed back through the API.
FTW stores them the same way it stores `weather.api_key`, and redacts them in
config responses.

Nothing below is Lantmaeteriet-specific beyond the defaults: search is the
standard `POST {base}/search` of the STAC API spec and downloads follow asset
hrefs, so any STAC-conformant catalog behind Basic auth (or none) works by
pointing `base_url` and the collection ids elsewhere. The one non-standard
wrinkle is the search bbox CRS -- the spec mandates WGS84, Lantmaeteriet
expects SWEREF 99 TM -- which is why callers choose the bbox they send (see
`--bbox-epsg` in __main__).

Only `requests` is used. The STAC API is plain JSON over HTTP, so pulling in
pystac-client would add a dependency for a search body we can write in six
lines -- and a thinner surface is easier to keep working when Lantmaeteriet
moves an endpoint.
"""

from __future__ import annotations

import dataclasses
import datetime as dt
from typing import Any, Iterable

# Roots of the STAC APIs, as verified against the live service (2026-09-02):
# Lantmaeteriet does not serve one catalogue — vector products and elevation
# products have separate STAC roots. The standard search endpoint is
# POST {base}/search on each, it takes a WGS84 (lon/lat) bbox per the STAC
# spec, and the catalogue metadata is anonymously readable; the Geotorget
# credentials are enforced on the asset downloads (dl1.lantmateriet.se
# answers 401 without them).
DEFAULT_BASE_URL = "https://api.lantmateriet.se/stac-vektor/v1"
DEFAULT_LIDAR_BASE_URL = "https://api.lantmateriet.se/stac-hojd/v1"

# Collection ids as published in the live catalogues. Buildings are one item
# per municipality whose asset is a ZIP holding a GeoPackage; "Laserdata Skog"
# is published as `dsm-skoglig-copc` — a surface-model point cloud in
# LAZ/COPC — on the elevation root.
COLLECTION_BUILDINGS = "byggnader"
COLLECTION_LIDAR = "dsm-skoglig-copc"

# Media types, so an asset is chosen by *what it is* rather than by hoping the
# publisher named the key "data". Byggnader delivers a zipped GeoPackage;
# Laserdata Skog delivers LAZ organised as COPC (Cloud Optimized Point Cloud).
MEDIA_GEOPACKAGE = "application/geopackage+sqlite3"
MEDIA_COPC = "application/vnd.laszip+copc"
MEDIA_LAZ = "application/vnd.laszip"
MEDIA_LAS = "application/vnd.las"
MEDIA_GEOJSON = "application/geo+json"
MEDIA_ZIP = "application/zip"

# Longest suffix first: a COPC file is also a .laz, and reading it as a plain
# one would download the whole tile instead of the part we asked for.
_EXTENSION_MEDIA: tuple[tuple[str, str], ...] = (
    (".copc.laz", MEDIA_COPC),
    (".gpkg", MEDIA_GEOPACKAGE),
    (".geojson", MEDIA_GEOJSON),
    (".laz", MEDIA_LAZ),
    (".las", MEDIA_LAS),
    (".zip", MEDIA_ZIP),
)


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
                "a STAC username and password are both required; for Lantmäteriet, "
                "order access at https://geotorget.lantmateriet.se and set "
                "roofmodel.stac_username and roofmodel.stac_password to your "
                "Geotorget account credentials"
            )


def media_type_for(href: str) -> str | None:
    """Media type implied by a URL's extension, or None if it says nothing."""
    path = href.split("?", 1)[0].split("#", 1)[0].lower()
    for suffix, media in _EXTENSION_MEDIA:
        if path.endswith(suffix):
            return media
    return None


@dataclasses.dataclass(frozen=True)
class Asset:
    """One STAC asset: where it is, and what it is."""

    href: str
    media_type: str | None = None
    roles: tuple[str, ...] = ()
    title: str = ""

    @property
    def effective_media_type(self) -> str | None:
        """Declared media type, or the one the extension implies.

        Catalogues are inconsistent about `type`, and an asset with no declared
        type is common enough that refusing to guess would mean refusing most
        real items. The extension is only consulted when nothing was declared.
        """
        return self.media_type or media_type_for(self.href)


@dataclasses.dataclass
class StacItem:
    """One STAC item, reduced to what the pipeline needs."""

    item_id: str
    collection: str
    assets: dict[str, Asset]
    captured_at: dt.datetime | None
    raw: dict[str, Any] = dataclasses.field(default_factory=dict, repr=False)

    def __post_init__(self) -> None:
        # A bare href is accepted wherever an Asset is, so callers and tests can
        # write {"data": "http://.../tile.copc.laz"} without losing the typing
        # that selection depends on -- the extension supplies it.
        self.assets = {
            name: value if isinstance(value, Asset) else Asset(href=str(value))
            for name, value in (self.assets or {}).items()
        }

    def pick(self, *media_types: str) -> Asset | None:
        """Best asset for a wanted media type, most preferred type first.

        Where nothing declares a usable type the search widens: an asset with
        the `data` role, then a lone asset, since an item carrying exactly one
        asset is unambiguous however it is labelled.

        Both fallbacks consider only assets of *unknown* type. Guessing in the
        absence of information is reasonable; guessing against it is not, and an
        item whose single asset is a thumbnail must not be handed back as a
        point cloud.
        """
        for wanted in media_types:
            for asset in self.assets.values():
                if asset.effective_media_type == wanted:
                    return asset
        untyped = [a for a in self.assets.values() if a.effective_media_type is None]
        for asset in untyped:
            if "data" in asset.roles:
                return asset
        if len(untyped) == 1:
            return untyped[0]
        return None

    def asset_url(self, *preferred: str) -> str | None:
        """First matching asset href, trying each preferred key in order."""
        for key in preferred:
            if key in self.assets:
                return self.assets[key].href
        first = next(iter(self.assets.values()), None)
        return first.href if first else None


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
        name: Asset(
            href=asset.get("href", ""),
            media_type=asset.get("type") or None,
            roles=tuple(asset.get("roles") or ()),
            title=str(asset.get("title") or ""),
        )
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


class StacClient:
    """Thin client for a STAC search-and-download API over HTTP Basic auth.

    Defaults target Lantmaeteriet's Geotorget catalog, but nothing here
    depends on it: base_url and the collection ids passed to `search` are the
    whole coupling.
    """

    def __init__(
        self,
        credentials: Credentials,
        session: Any = None,
        base_url: str = DEFAULT_BASE_URL,
        timeout: float = 60.0,
    ) -> None:
        self._base_url = base_url.rstrip("/")
        # Lantmäteriet always demands the operator's Geotorget account, so
        # missing credentials there deserve the ordering instructions early.
        # A custom catalog may be open: no credentials means anonymous access,
        # while half a credential is still an error either way.
        self._anonymous = not (credentials.username or credentials.password)
        if not self._anonymous or self._base_url in (DEFAULT_BASE_URL, DEFAULT_LIDAR_BASE_URL):
            credentials.validate()
        self._credentials = credentials
        self._timeout = timeout
        if session is None:
            import requests  # imported lazily so tests can inject a fake session

            session = requests.Session()
            if not self._anonymous:
                session.auth = (credentials.username, credentials.password)
        self._session = session

    @property
    def session(self) -> Any:
        """The authenticated session, for readers that stream their own ranges."""
        return self._session

    def search(
        self,
        collection: str,
        bbox: tuple[float, float, float, float],
        limit: int = 20,
    ) -> list[StacItem]:
        """POST {base}/search for one collection over a bbox.

        The bbox is (min_x, min_y, max_x, max_y) in whatever CRS the catalog
        expects -- the STAC spec says WGS84 lon/lat, and the live Lantmaeteriet
        service follows it -- so the caller chooses what to send and no
        reprojection happens here.
        """
        body = {
            "collections": [collection],
            "bbox": list(bbox),
            "limit": limit,
        }
        url = f"{self._base_url}/search"
        try:
            resp = self._session.post(url, json=body, timeout=self._timeout)
        except Exception as exc:  # network, DNS, TLS
            raise GeotorgetError(f"STAC search failed: {exc}") from exc
        if resp.status_code in (401, 403):
            if self._anonymous:
                raise MissingCredentials(
                    f"the STAC catalog requires credentials for {collection} "
                    f"(HTTP {resp.status_code}). Set roofmodel.stac_username "
                    "and roofmodel.stac_password for this catalog."
                )
            raise MissingCredentials(
                f"the STAC catalog rejected the credentials for {collection} "
                f"(HTTP {resp.status_code}). Check the username and password, and "
                "that the account has ordered access to this product."
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


# The client predates its generalization; the old name stays importable.
GeotorgetClient = StacClient

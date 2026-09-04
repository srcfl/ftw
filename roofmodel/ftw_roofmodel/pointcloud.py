"""Read Lantmaeteriet LiDAR, fetching only the part of the tile we need.

*Laserdata Nedladdning, Skog* is delivered as LAZ organised as **COPC** (Cloud
Optimized Point Cloud): the points are ordered into an octree and the node index
lives in a VLR at a known offset, so a reader that can issue HTTP range requests
can pull the handful of octree nodes covering one building instead of the whole
2.5 km tile.

That is worth real money on a Pi. A Laserdata Skog tile is hundreds of megabytes;
a detached house is a few tens of metres across. Since the operator has already
told us *which building* they mean, the bounding box of that footprint is exactly
the query COPC exists to answer.

The fallbacks are deliberate and ordered, because none of the preconditions are
guaranteed:

  1. COPC asset + a bounding box + a server that honours `Range` -> spatial query.
  2. Anything else -> download the asset and read it whole.

A server that ignores `Range` returns 200 with the entire body, which would
otherwise be mistaken for a successful partial read, so that case is detected on
the status code rather than assumed away.
"""

from __future__ import annotations

import io
from typing import Any

__all__ = [
    "PointCloudError",
    "HttpRangeFile",
    "bounds_of",
    "copc_query_z_range",
    "load_points",
    "read_copc_window",
]

# Read this much per range request. COPC chunks are small, and a request per
# chunk would spend more time in round trips than in transfer.
DEFAULT_CHUNK_BYTES = 1 << 20


class PointCloudError(RuntimeError):
    """The LiDAR asset could not be read."""


class HttpRangeFile(io.RawIOBase):
    """A seekable read-only file over HTTP `Range` requests.

    laspy's COPC reader needs `seek`/`read` and nothing else, so this is the
    whole adapter: it turns an HTTP URL into something that behaves like an open
    file without ever holding the tile in memory.
    """

    def __init__(self, session: Any, url: str, *, timeout: float = 60.0,
                 chunk_bytes: int = DEFAULT_CHUNK_BYTES) -> None:
        self._session = session
        self._url = url
        self._timeout = timeout
        self._chunk = max(1, chunk_bytes)
        self._pos = 0
        self._size: int | None = None
        # One cached chunk. COPC reads are clustered -- header, then index, then
        # the nodes -- so a single block absorbs most of the repeat traffic.
        self._cache: tuple[int, bytes] | None = None
        self.requests = 0
        self.bytes_fetched = 0

    # -- io.RawIOBase ----------------------------------------------------
    def readable(self) -> bool:
        return True

    def seekable(self) -> bool:
        return True

    def tell(self) -> int:
        return self._pos

    def seek(self, offset: int, whence: int = io.SEEK_SET) -> int:
        if whence == io.SEEK_SET:
            self._pos = offset
        elif whence == io.SEEK_CUR:
            self._pos += offset
        elif whence == io.SEEK_END:
            self._pos = self.size + offset
        else:  # pragma: no cover - io module only defines the three
            raise ValueError(f"invalid whence {whence}")
        self._pos = max(0, self._pos)
        return self._pos

    def read(self, size: int = -1) -> bytes:
        if size is None or size < 0:
            size = max(0, self.size - self._pos)
        if size == 0:
            return b""
        data = self._read_at(self._pos, size)
        self._pos += len(data)
        return data

    def readall(self) -> bytes:
        return self.read(-1)

    def readinto(self, buffer) -> int:  # type: ignore[override]
        data = self.read(len(buffer))
        buffer[: len(data)] = data
        return len(data)

    # -- range plumbing --------------------------------------------------
    @property
    def size(self) -> int:
        if self._size is None:
            self._size = self._head_size()
        return self._size

    def _head_size(self) -> int:
        try:
            resp = self._session.head(self._url, timeout=self._timeout)
        except Exception as exc:
            raise PointCloudError(f"could not stat {self._url}: {exc}") from exc
        length = (getattr(resp, "headers", None) or {}).get("Content-Length")
        if getattr(resp, "status_code", 0) != 200 or not length:
            raise PointCloudError(
                "the LiDAR host did not report a size, so it cannot be read in ranges"
            )
        return int(length)

    def _read_at(self, offset: int, size: int) -> bytes:
        cached = self._from_cache(offset, size)
        if cached is not None:
            return cached
        want = max(size, self._chunk)
        end = offset + want - 1
        try:
            resp = self._session.get(
                self._url,
                headers={"Range": f"bytes={offset}-{end}"},
                timeout=self._timeout,
            )
        except Exception as exc:
            raise PointCloudError(f"range request failed: {exc}") from exc
        status = getattr(resp, "status_code", 0)
        if status == 200:
            # The server ignored Range and sent everything. Honest failure: the
            # caller falls back to a whole-tile read rather than silently
            # paying for the full download on every seek.
            raise PointCloudError("the LiDAR host does not support range requests")
        if status != 206:
            raise PointCloudError(f"range request returned HTTP {status}")
        body = resp.content
        self.requests += 1
        self.bytes_fetched += len(body)
        self._cache = (offset, body)
        return body[:size]

    def _from_cache(self, offset: int, size: int) -> bytes | None:
        if self._cache is None:
            return None
        start, body = self._cache
        if offset < start or offset + size > start + len(body):
            return None
        rel = offset - start
        return body[rel : rel + size]


def bounds_of(ring: list[tuple[float, float]], pad_m: float = 2.0) -> tuple[float, float, float, float]:
    xs = [p[0] for p in ring]
    ys = [p[1] for p in ring]
    return (min(xs) - pad_m, min(ys) - pad_m, max(xs) + pad_m, max(ys) + pad_m)


def _points_from_las(las: Any) -> Any:
    import numpy as np

    return np.column_stack([np.asarray(las.x), np.asarray(las.y), np.asarray(las.z)])


def load_points(data: bytes) -> Any:
    """Decode a whole LAZ/LAS payload into (N, 3) SWEREF 99 TM metres."""
    laspy = _import_laspy()
    with laspy.open(io.BytesIO(data)) as reader:
        las = reader.read()
    return _points_from_las(las)


def _import_laspy():
    try:
        import laspy
    except ImportError as exc:  # pragma: no cover - depends on the install
        raise PointCloudError(
            "laspy is required to read Lantmaeteriet LiDAR. Install the module's "
            "extras: pip install -e roofmodel[geo]"
        ) from exc
    return laspy


def copc_query_z_range(center_z: float, halfsize: float) -> tuple[float, float]:
    """The vertical range a windowed COPC query must span: the whole cube.

    Lantmäteriet's COPC writer emits octree z-keys measured from some other
    origin than the file's own cube (observed live on Laserdata Skog: a level-6
    node keyed z=19, implying a -1698..-1542 m slab, holds points at +18..+42 m
    while its x/y keys are exact). A 2D query lets laspy fill z from the header
    and prune nodes by those broken slabs, which silently discards every dense
    deep level and leaves only the sparse preview points. Spanning the full
    cube keeps z from ever pruning; x/y pruning and the exact post-filter still
    bound the read.
    """
    pad = abs(halfsize) + 1.0
    return (center_z - 2.0 * pad, center_z + 2.0 * pad)


def read_copc_window(
    session: Any,
    url: str,
    bounds: tuple[float, float, float, float],
    *,
    timeout: float = 60.0,
) -> Any:
    """Points inside `bounds` from a COPC file, over HTTP range requests.

    Raises PointCloudError if the file or the host cannot support it, so the
    caller can fall back to a whole-tile read.
    """
    laspy = _import_laspy()
    try:
        from laspy.copc import Bounds, CopcReader
    except ImportError as exc:
        raise PointCloudError(
            "this laspy build has no COPC support; install laspy[lazrs] 2.5 or newer"
        ) from exc

    min_x, min_y, max_x, max_y = bounds
    handle = HttpRangeFile(session, url, timeout=timeout)
    try:
        with CopcReader.open(handle) as reader:
            info = reader.copc_info
            z_lo, z_hi = copc_query_z_range(float(info.center[2]), float(info.halfsize))
            query = Bounds(mins=[min_x, min_y, z_lo], maxs=[max_x, max_y, z_hi])
            points = reader.query(query)
    except PointCloudError:
        raise
    except Exception as exc:
        raise PointCloudError(f"COPC read failed: {exc}") from exc
    return _points_from_las(points)

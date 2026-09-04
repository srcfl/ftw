"""FTW roof-geometry module.

Derives PV array geometry (tilt, azimuth, kWp) from Lantmaeteriet open geodata.
Optional and independently versioned: core reads only the versioned
`roof_model.json` this module emits, and works normally without it.
"""

from .pipeline import SCHEMA_VERSION, RoofModelError, derive, planes_to_arrays
from .segment import RoofPlane, segment_roof

__all__ = [
    "SCHEMA_VERSION",
    "RoofModelError",
    "RoofPlane",
    "derive",
    "planes_to_arrays",
    "segment_roof",
]

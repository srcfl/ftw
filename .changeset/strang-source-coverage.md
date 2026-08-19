---
"ftw": minor
---

SMHI STRÅNG becomes a first-class irradiance source, every external data source
now declares where in the world it works, and the location picker moves to
MapLibre GL JS.

- **STRÅNG as an irradiance source.** The client now covers the model's full
  parameter set and knows its own domain. A nightly backfill scores measured
  production against the DC energy the configured arrays should have produced
  under that irradiance, exposed at `GET /api/pv/performance` and drawn as a
  dashed "expected (STRÅNG)" overlay on the Produced tile. The resulting
  performance ratio feeds back as a calibration factor on the forward forecast,
  refused outright when it lands outside a plausible band — a site reading at
  10% or 160% of nameplate is a configuration fault, and silently rescaling the
  forecast would hide it.

- **Cloud cover, derived.** STRÅNG publishes no cloud-cover parameter; it is a
  radiation model. It does publish sunshine duration (minutes per hour above the
  WMO beam threshold), so cloudiness is recovered as `1 − minutes/60`. That is an
  observed quantity rather than an inferred cloud field, but coarser: blind to
  thin cirrus, and undefined at night. The API returns an explicit *unknown*
  rather than defaulting to *clear*, because those lead to opposite decisions.

- **Coverage metadata.** New `GET /api/data-sources` reports every forecast,
  irradiance and price source with its coverage area, country list, licence and
  whether it reaches this specific site; the Weather tab renders it under the
  map and flags sources that do not. This makes an existing silence explicit:
  STRÅNG is Nordic-only and every price provider is European, so sites elsewhere
  were getting empty results with no explanation. Bounds are advisory — a
  rotated model grid means a lat/lon box can only be a superset — so `false` is
  definitive and `true` means "worth trying". PV performance scoring now declines
  to start outside the STRÅNG domain instead of retrying nightly forever.

- **MapLibre GL JS location picker.** The Weather tab's map is now MapLibre GL
  JS 6 (BSD-3) instead of Leaflet, still lazy-loaded only when the tab opens. v6
  is ESM-only and code-split, so it loads via a pinned dynamic import; the
  stylesheet keeps its integrity hash, while the JS relies on version pinning
  (an integrity hash on the entry point would not cover the shared chunk it
  imports anyway). The style is built inline from the same OpenStreetMap raster
  tiles as before, so neither the tile source nor the attribution changed. The
  numeric latitude/longitude fields remain authoritative, so a CDN or WebGL
  failure costs the picker and nothing else.

Nothing here touches the control tick, dispatch or the optimizer contract.

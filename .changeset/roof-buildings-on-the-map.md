---
"ftw": patch
---

Found buildings now actually appear on the map, and clicking one selects it.
Three defects hid them: MapLibre silently rejected both footprint layers
because the styling used a value-typed `["get", "selected"]` where a typed
boolean is required, and — once typed — because the theme's `oklch()` colour
tokens reached MapLibre's parser unconverted, so the resolved colours are now
baked to sRGB through a canvas probe. Finally the camera never moved: the map
sat at city zoom, where a footprint is smaller than a pixel. A find now zooms
to the nearby candidates, and clicking a footprint selects that building
without also dragging the site pin (and its saved coordinates) to wherever
you clicked.

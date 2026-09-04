---
"ftw": minor
---

PV arrays can be drawn on the map instead of measured by hand. The Weather tab gains a rectangle tool over the existing MapLibre map: draw over your panels and it becomes a `weather.pv_arrays` entry, with area from the shape and azimuth from the way the shape is turned. Capacity is stored as `rated_w` (watts).

The rectangle is drawn at an angle rather than square to north, because the angle is the point — the long edge follows the ridge, and the face is perpendicular to it. Two directions are perpendicular to a ridge and a flat outline genuinely does not say which, so the equatorward one is offered as a default with a one-click flip, never as a measurement.

Tilt is the one number an overhead outline cannot contain, so it is typed once before drawing — and it is also what turns the outline into panel area. What you trace on a map is the horizontal projection of a sloped rectangle, so a 35° roof carries about 22 % more panel than its outline suggests; ignoring that would quietly under-size every drawn array. Capacity uses the same packing factor and module density as the Lantmäteriet roof model, so a drawn array and a derived one are comparable.

Drawing is progressive enhancement: [Terra Draw](https://github.com/JamesLMilner/terra-draw) and its MapLibre adapter (both MIT) are lazy-loaded from CDN with SRI hashes only when the tool is first used, and if either fails to load the numeric editor is untouched and says so. The editor stays the final word for drawn and typed arrays alike.

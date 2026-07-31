---
"ftw": minor
---

Roof faces can now carry a shading factor from neighbouring buildings and trees,
computed with vostok.

Roof segmentation gives each face a tilt and an azimuth, which predicts yield for
an unobstructed roof and says nothing about the spruce to the south. `vostok`
("Voxel Octree Solar Toolkit") computes per-point solar potential against
voxelised occlusion geometry, so pointing it at the same LiDAR tile the roof came
from supplies exactly the missing number. Each face is run twice — once against
the real geometry, once with shadowing off — and the ratio is a shading factor
that is independent of vostok's absolute units, sky model and chosen year, since
all of those cancel.

**vostok is GPL-3.0 and FTW is not.** It is therefore treated as an external
program at arm's length: a separate process communicating through files and a
command line, never linked, with no code copied in either direction. Two rules
keep that boundary intact — vostok is never bundled or redistributed with FTW,
and FTW never installs it. The operator installs it themselves and sets
`roofmodel.vostok_binary`.

Absent or unset, shading is simply not evaluated: faces report `shading_factor`
as *absent* rather than 1.0, because "we did not look" and "we looked and it is
unobstructed" are different claims and only one of them justifies trusting the
yield estimate. A missing or failing binary never fails a derive — the roof
geometry is still good without a shading number.

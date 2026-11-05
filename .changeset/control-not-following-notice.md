---
"ftw": minor
---

A phone now hears when a device does not do what FTW asked. The new
`control.not_following` notification fires when the box's answer to "Are we
in control?" has said a device is not following for ten minutes, as when a
battery charges while FTW asks it to discharge. It fires once until the
device follows again, and at most once an hour. It is on by default for
owners who have notifications on, and each owner can turn it off with the
other notification choices.

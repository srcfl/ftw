---
"ftw": patch
---

When the box refuses to let the home battery charge the car, it now says
why. The app's refusal (`E_UNAVAILABLE` for `loadpoint.boost`) carries a
`reason` in the same words as a stopped boost, such as
`battery_reserve_reached`, so the app can say that the battery is too low
instead of only that it cannot help.

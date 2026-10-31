---
"ftw": minor
---

Store the home's time zone on the site. Car goals, the import hour, daily history and savings now read the site's `site.time_zone` instead of the host's clock, so a Docker box on UTC meets a 17:00 goal at 17:00 at home. The app, the web client and the on-box setup offer their own zone to a box that keeps none, and the box restarts once to run on it. A box that already keeps a zone, stored or from its host, keeps it; change it under Settings → Site.

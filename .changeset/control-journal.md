---
"ftw": minor
---

The box keeps a control journal. Every few seconds it records the answer to
"Are we in control?" for each controlled device, each source, whether the site
may dispatch and the operating mode; a change starts a new stretch, with the
power Core asked for and the power delivered while it held. Each scored plan
slot is kept too, with its planned and delivered energy. Read it over any
window with GET /api/control/journal, and as an agent with events.get, so the
owner, an assistant or support can learn afterwards why the app warned. A
notification the database was too busy to record is now written once it is
free.

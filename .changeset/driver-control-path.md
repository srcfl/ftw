---
"ftw": minor
---

An operator can now send a driver's declared command and hold it for a bounded
time. `POST /api/drivers/{name}/control` takes `{control, value, duration_s}`;
`DELETE` on the same path ends the hold early. The active hold appears on
`/api/drivers/{name}` so a UI can show what is set and until when.

Deliberately outside control v2. A signed package binds a RuntimePolicy and
goes through `CommandV2` with its write scope, lease and evidence, unchanged.
A bundled or local driver has no policy, and synthesising one would be worse
than doing nothing: `HostEnv.permissionAllowed` grants everything only while
the policy is nil, so a policy without permissions silently blocks the driver's
own MQTT, and `LuaDriver.Command` refuses a control v2 driver on the legacy
path — v2 wants `driver_command_v2` entrypoints no community driver has. This
path leaves the policy layer untouched and validates against the catalog
declaration instead.

What that costs, stated plainly: no host-enforced write scope, no host-verified
evidence. What it keeps is the part that protects hardware. Core clamps every
value to the declared bounds rather than trusting the Lua to do it — a driver
that forgets to clamp is exactly the driver this protects — and the driver's
own declaration is the whole allowlist, so an undeclared control is a 400
rather than a 200 for a command the Lua silently ignored.

Every hold ends by itself, and ending means calling the driver's own
`driver_default_mode` rather than writing a value Core invented: only the
driver knows what neutral is. Default 4 h, maximum 24 h, and nothing survives a
restart. On process start or driver re-add, a legacy driver must also confirm
that default before control opens; a failed confirmation keeps control blocked
and retries with a bounded backoff. An offset left behind by a browser tab that
closed is a house heated wrong for weeks.

---
"ftw": minor
---

A read-only MQTT driver from the signed channel may now send the read
requests its signed policy declares, such as Victron's
`R/<portal ID>/keepalive`.

Venus OS on a Victron GX publishes its values only while a client sends that
keep-alive. FTW refused every publish from a signed read-only driver, so
`victron_mqtt` from the channel went stale on a GX with no other MQTT client.

Core now lets such a driver publish to a topic its signed manifest lists, and
to nothing else. The grant needs `mqtt.subscribe` and no write permission, so
an older Core runs the same signed driver read-only and refuses the request.
A listed topic must start with `R/`, Venus's read-request namespace; a `+`
stands for one whole level, and `#` is refused. Each request carries at most
1 KiB, and a driver may send six a minute. Setup measurement mode holds read
requests as it holds writes. Every other publish, Modbus write and POST from a
read-only driver is still refused. Bundled, local and control drivers behave
as before.

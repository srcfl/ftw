---
"ftw": patch
---

The per-phase fuse guard now sizes its correction from the site's own phase count and voltage. When one phase runs over the breaker, the guard converts that overage into battery watts, and it used to assume every site had three phases and 230 V per phase. On a single-phase site that asked the battery for three times the relief the site needed, enough to push the meter through zero and into a violation the other way; the phase count and voltage the operator configured now do the conversion. The guard also reads only the phases the site has: current reported on an L2 that a single-phase site does not own no longer fires it. A site whose fuse is described by amps alone gets no per-phase clamp rather than one computed from invented numbers — the same rule the fuse safety margin already follows. Three-phase 230 V sites, which is what configuration defaults to, are unaffected.

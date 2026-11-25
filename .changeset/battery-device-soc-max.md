---
"ftw": patch
---

Plan and charge a battery no higher than the charge level its own device
reports it will accept. A Pixii can stop at 90 % while the plan booked energy
up to 95 %, so dispatch ordered charge the battery never took. Drivers report
the limit as `max_soc_fract` (Pixii from driver 2.2.0). Core uses the lower of
it and the configured ceiling; a configured ceiling below it still applies.

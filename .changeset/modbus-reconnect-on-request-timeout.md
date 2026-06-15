---
"forty-two-watts": patch
---

Fix Modbus drivers getting stuck after a device goes mute on a long-lived
session. The reconnect classifier (`isTransportError`) recognised closed-socket
errors but not `simonvetter/modbus`'s own deadline sentinel `ErrRequestTimedOut`
("request timed out") — a plain string-typed value that is neither a `net.Error`
nor wraps `syscall.ETIMEDOUT`. When a device kept the TCP socket `ESTABLISHED`
but stopped answering requests on it, every read/write timed out and the wrapper
reused the dead socket forever instead of redialing.

Seen in the field on a CTEK Chargestorm CSOS charger: 2907 consecutive
charge-limit writes timed out over ~43 h, so the controller could never set the
EV charge current and the loadpoint never followed PV surplus — while a fresh
connection to the same charger read and wrote the register instantly. The
classifier now treats the timeout sentinel as a transport error, so the next
call tears down the wedged socket and dials a fresh one.

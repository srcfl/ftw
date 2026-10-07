---
"ftw": patch
---

Keep Test connection from breaking an OAuth driver's login. Settings sends the driver's credential owner, and the probe then missed its own token writes: it tested the saved token instead of a newly pasted one, and rotated the live token without restarting the running driver.

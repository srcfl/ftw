---
"ftw": patch
---

After a restart, one high site reading no longer holds an idle car back for close to a minute. The fuse heat model used to start at the first reading's current, as if that overload had lasted. In seed 207539 one reading of 27.94 A on a 20 A fuse delayed the car's start by about 43 s. The model now starts at the first reading's current, at most the working limit. A sustained overload after a restart is still cut on the next reading, and readings above 1.2 × the fuse rating are still cut at once.

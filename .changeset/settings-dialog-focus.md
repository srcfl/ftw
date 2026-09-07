---
"ftw": patch
---

Move focus into Settings when it opens, keep Tab within its visible controls,
and let Escape close it. Closing Settings returns focus to its opening button,
including the shortcut in More. The restart prompt keeps focus while open,
blocks the background, and returns focus when Restart later or Escape closes
the prompt. During a pending restart, focus stays in the prompt and Escape
does not close it.
Show restart progress only after Restart now starts the request.

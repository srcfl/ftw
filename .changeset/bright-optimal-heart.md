---
"ftw": minor
---

Replace the discretized Go MPC as the primary planner with a CVXPY mathematical optimizer using HiGHS for LP/MILP, CLARABEL for continuous convex fallback, scenario/CVaR forecast risk, independently constrained battery fleets, recoverable out-of-band SoC states, multiple jointly optimized EV loadpoints, Go-side trajectory validation, deterministic and historical replay tooling, an always-on diagnostic DP shadow, and automatic Go-DP emergency fallback.

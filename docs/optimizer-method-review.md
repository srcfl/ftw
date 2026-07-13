# Edge optimizer method review

Reviewed 2026-07-13. This document deliberately separates forecast quality
from decision quality. The forecast/scenario generator is the next workstream;
this review asks whether the optimization and control method is appropriate
given a set of inputs.

## Verdict

The system has the right top-level shape for a production edge HEMS:

1. a slow receding-horizon planner for economic scheduling;
2. Go-side validation, watchdogs, fuse/export bounds, and deterministic fallback;
3. a fast telemetry-driven regulator that executes energy budgets rather than
   blindly replaying forecast grid power;
4. shadow policies that cannot command hardware.

That hierarchy is stronger than replacing dispatch with an end-to-end learned
policy. In a real residential test, MPC and well-designed rule/tree controllers
were within 0.6%, while the still-training RL controller cost 25.5% more. The
same study also found that every method remained sensitive to implementation
and model errors ([Ruddick et al., 2024](https://doi.org/10.1016/j.egyai.2024.100448)).

The optimizer itself was not state of the art in uncertainty handling. Its
three PV scenarios all shared one asset schedule for the full 48-hour horizon.
That is a robust open-loop schedule, not stochastic recourse: tomorrow's
battery decision could not adapt even though the controller will have received
new measurements and forecasts by then. Residential stochastic MPC literature
uses scenario-dependent futures with non-anticipativity where information is
still shared ([van der Meer et al., 2021](https://doi.org/10.1016/j.apenergy.2020.116289)).

This branch adds a two-stage storage-recourse challenger with a shared first
15-minute action and stateful closed-loop scoring. It closes the largest method
gap without changing dispatch. It does **not** make the project fully SOTA yet:
two-stage recourse assumes the rest of each scenario path becomes known after
the first stage and is therefore an optimistic bound. A small multi-stage tree
is the eventual production target.

## Method scorecard

| Area | Current assessment | Target |
|---|---|---|
| Safety and fallback | Strong | Keep Go as final authority |
| Asset/constraint coverage | Strong champion; recourse is storage-only | Scenario-dependent EV and thermal state |
| Uncertainty policy | Shared open-loop champion + two-stage shadow | Small calibrated multi-stage tree |
| Closed-loop evidence | Stateful champion/challenger added | 7-14 day gates plus seasonal corpus |
| Edge compute | Acceptable with idle worker; high cold/RSS peak | Cached model and sparse direct fast path |
| Long horizon | Uniform 15-minute grid | Multi-resolution horizon/move blocking |
| Risk | Expected cost + CVaR knob | Risk on violations; economics mostly expected cost |

## Recommended production formulation

Use stochastic MPC, solved as a deterministic-equivalent LP/MILP while the
scenario set is small:

- 15-minute shared first action. Only this action can be dispatched.
- A scenario tree over the next 2-6 hours, where decisions share a node until
  their observations diverge.
- Coarser 30-60 minute blocks farther out. Move blocking is an established way
  to reduce online MPC variables, but its feasibility effect must be tested
  rather than assumed ([Shekhar and Manzie, 2015](https://doi.org/10.1016/j.automatica.2015.07.030)).
- Expected economic cost for normal operation. Apply chance constraints,
  CVaR, or distributional robustness to explicit violation risks such as SoC,
  fuse headroom, deadline shortfall, and outage reserve. Do not stack a wide
  scenario envelope and a large cost-CVaR term without calibration.
- Terminal value or a terminal band validated by closed-loop replay. Never
  compare horizon objectives with different terminal energy without valuing
  that energy consistently.

Conformal forecast intervals are a promising scenario input because they can
provide distribution-free calibration. A 280-day energy-hub study found its
scenario MPC about 0.8% better than deterministic point-forecast MPC, while still
11% above perfect forecast, illustrating both the value and the practical
ceiling of uncertainty handling ([Fernandez-Zapico et al., 2025](https://arxiv.org/abs/2504.00685)).
That belongs to the next forecast workstream, not this optimizer change.

Distributionally robust MPC is worth revisiting once enough residual history
exists to tune and validate an ambiguity set. Wasserstein formulations can
protect against distribution shift, but published results also show larger
ambiguity radii becoming overly conservative; it is not a free upgrade
([Recke and Hudoba de Badyn, 2026](https://arxiv.org/abs/2605.14642),
[Li et al., 2024](https://arxiv.org/abs/2403.16402)).

## Edge compute strategy

Keep CVXPY as the executable reference model and HiGHS as the primary solver.
HiGHS is designed for sparse LP/MIP/QP and has native Python and C interfaces
([HiGHS project](https://highs.dev/)). CLARABEL remains a useful continuous
convex fallback, not a MILP fallback.

The next compute improvements should be made in this order:

1. Vectorize CVXPY expressions and build a DPP-compliant parameterized problem.
   CVXPY then caches canonicalization for repeated solves
   ([CVXPY DPP guide](https://www.cvxpy.org/tutorial/dpp/index.html)).
2. Use a multi-resolution horizon so model size follows decision value, not
   simply `48 h / 15 min`.
3. Reduce a larger raw scenario ensemble to roughly 5-20 representative paths
   and measure out-of-sample coverage and value.
4. Add a direct sparse HiGHS battery-only fast path, continuously checked
   against the CVXPY champion. Retain CVXPY for complex assets and fallback.
5. Consider Progressive Hedging only when the scenario tree is too large for a
   monolithic deterministic equivalent. It adds convergence/tuning complexity
   and is primarily a scaling tool, not an accuracy feature
   ([Bastin et al., 2013](https://optimization-online.org/2013/10/4065/)).

## Promotion gate

A challenger may replace the champion only when all of the following hold:

- at least 7 complete days and 500 evaluated intervals, including a mix of PV
  and no-PV periods;
- lower realized import/export cost after identical terminal-energy valuation;
- no increase in SoC, fuse, export, or mode clamps;
- solve p95 and memory peak fit the supported Pi budget;
- no optimizer/validation fallback regression;
- the result reproduces on the stored replay corpus, not only one live site.

The planned objective is diagnostic. Promotion is based on stateful realized
score and safety metrics.

## Current branch contract

- `optimizer_recourse_shadow: false` by default.
- When enabled, champion and challenger run sequentially in the same warm
  worker, avoiding a second resident CVXPY process.
- The challenger has scenario-specific storage, grid, and PV-curtailment state
  after `optimizer_recourse_non_anticipative_slots` (default 1).
- Flexible EV/thermal contracts pause the challenger until their counterfactual
  state can be evaluated correctly.
- `/api/mpc/plan` and persisted diagnostics expose raw and terminal-valued
  challenger scores. The evaluator refuses to score an anticipative tail when
  a fresh shared-prefix plan is unavailable. Dispatch reads only the champion.

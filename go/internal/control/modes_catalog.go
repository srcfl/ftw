package control

// ModeTier groups modes by how the operator dashboard surfaces them.
type ModeTier string

const (
	// TierPrimary — the recommended forecast-driven strategies, shown up
	// front as the main strategy buttons.
	TierPrimary ModeTier = "primary"
	// TierAdvanced — manual fallbacks, shown behind the "Manual…" toggle.
	TierAdvanced ModeTier = "advanced"
	// TierHidden — valid modes that exist for the API / HA / planner
	// machinery but are intentionally not rendered as dashboard buttons.
	TierHidden ModeTier = "hidden"
)

// ModeInfo is one mode's operator-facing presentation: its stable key plus
// the label, tooltip, and tier the dashboard renders. It is the single
// source of truth for how a mode is shown, so the web UI no longer carries
// its own hand-maintained button list. The JSON shape is the /api/modes
// contract consumed by web/next-app.js.
type ModeInfo struct {
	Key     Mode     `json:"key"`
	Label   string   `json:"label"`
	Tooltip string   `json:"tooltip"`
	Tier    ModeTier `json:"tier"`
}

// ModeCatalog returns every operator-selectable mode annotated with its
// label, tooltip, and tier, in dashboard presentation order (primary first,
// then advanced, then hidden). The dashboard builds its mode buttons from
// this list instead of hard-coding them; primary + advanced render as
// buttons (advanced behind the "Manual…" toggle), hidden modes stay valid
// but unshown.
//
// Invariant: exactly one entry per AllModes() value — no missing, extra, or
// duplicate keys. TestModeCatalogCoversAllModes enforces it, so a new mode
// added to the enum can't ship without a deliberate presentation decision
// here. That's the UI-side counterpart to the HA discovery fix: both the
// dropdown and the dashboard now derive from the same canonical mode set.
func ModeCatalog() []ModeInfo {
	return []ModeInfo{
		// Primary — forecast-driven strategies (the default choices).
		{ModePlannerPassiveArbitrage, "Passive arbitrage", "Charge from the cheapest available source (PV when sunny, grid during cheap hours). Never exports from battery.", TierPrimary},
		{ModePlannerArbitrage, "Active arbitrage", "Full price arbitrage — charge cheap, discharge into expensive hours (battery may export to grid).", TierPrimary},
		// Advanced — manual fallbacks behind the "Manual…" toggle.
		{ModeIdle, "Idle", "Do nothing — no dispatch.", TierAdvanced},
		{ModeSelfConsumption, "Self (manual)", "Manual self-consumption — PI chases grid target, no plan.", TierAdvanced},
		{ModePeakShaving, "Peak", "Limit grid import to the configured peak limit.", TierAdvanced},
		{ModeCharge, "Charge", "Force full charge regardless of price.", TierAdvanced},
		// Hidden — valid (API + HA + planner) but not surfaced as buttons.
		{ModePlannerSelf, "Planner (self)", "Forecast-driven self-consumption — never grid-charges, never exports.", TierHidden},
		{ModePlannerCheap, "Planner (cheap)", "Forecast-driven — grid-charges during cheap hours, never exports.", TierHidden},
		{ModePriority, "Priority", "Fill the highest-priority battery first.", TierHidden},
		{ModeWeighted, "Weighted", "Distribute dispatch across batteries by configured weights.", TierHidden},
	}
}

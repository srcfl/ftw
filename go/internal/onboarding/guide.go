package onboarding

import "github.com/srcfl/ftw/go/internal/config"

type Question struct {
	ID          string   `json:"id"`
	Prompt      string   `json:"prompt"`
	Paths       []string `json:"paths"`
	RequiredFor string   `json:"required_for,omitempty"`
	Unit        string   `json:"unit,omitempty"`
	Help        string   `json:"help,omitempty"`
}

// Guide uses the same Config type as Settings. The box, rather than Cloud,
// decides which facts are missing. Optional unknowns never prevent saving.
func Guide(c *config.Config) map[string]any {
	questions := []Question{}
	add := func(id, prompt, need, unit, help string, paths ...string) {
		questions = append(questions, Question{id, prompt, paths, need, unit, help})
	}
	if c.Site.Name == "" {
		add("home_name", "What would you like to call this home?", "", "", "Use the box identity to confirm the right home before editing.", "/site/name")
	}
	if c.Setup == nil || !c.Setup.ConnectionConfirmed || c.Setup.ConnectionDigest != c.SetupConnectionDigest() {
		add("electrical_connection", "What main fuse and connection does your electricity network contract show?", "automatic_control", "A; phases; V per phase", "Do not guess from the country. A three-phase 400/230 V supply uses voltage=230 here. Keep measurement mode on if unknown. Confirm the exact returned connection_digest after reviewing these values.", "/fuse", "/setup/connection_confirmed", "/setup/connection_digest")
	}
	if c.SiteMeterDriver() == "" {
		add("site_meter", "Which meter measures this whole home's import and export at the grid boundary?", "automatic_control", "", "Offer explicit network discovery, or manual entry from the driver catalogue. A found device is not yet selected or tested.", "/drivers")
	}
	if c.Setup == nil || !c.Setup.EquipmentReviewed {
		add("equipment", "Which equipment should FTW use: meter, solar, battery, chargers, cars, heating or other integrations?", "automatic_control", "", "Read detected facts and catalogue fields before asking. Configure all requested devices and integrations with the schema. An optional offline device can remain for later; report it clearly.", "/drivers", "/batteries", "/ev_charger", "/loadpoints", "/vehicles", "/ocpp", "/homeassistant", "/notifications", "/setup/equipment_reviewed")
	}
	if c.Setup == nil || c.Setup.SolarPresent == nil {
		add("solar", "Does this home have solar panels? If so, do you know their total kWp?", "", "kWp (convert to W for pv_rated_w)", "Capacity and roof orientation are optional estimates. Record their source and time in forecast_priors.solar_source and solar_recorded_at. They are not verified inverter control limits. Do not ask for a capacity already known from reliable device metadata.", "/setup/solar_present", "/weather/pv_rated_w", "/weather/pv_arrays")
	}
	if c.Setup == nil || !c.Setup.GoalsReviewed {
		add("goals", "What should FTW do for this home: use solar, shift energy to cheap hours, keep a battery reserve, or prepare cars for departure?", "", "W, Wh; SoC as 0..1", "Read the mode catalogue in setup_schema and active mode/preferences in commissioning. Confirm runtime_timezone before any local HH:MM departure target. If it is wrong, set /site/time_zone to the household's IANA zone; Core applies it when it restarts. Store equipment limits and car/loadpoint configuration in this draft. After configuration is running and measurements are fresh, while measurement mode still blocks equipment control, use car_goal_request for car departure schedules and goal_request for the chosen operating mode and planning style, including explicit battery-export permission. The owner approves each request in Access on the local box or in the FTW app; read requests_list and active status to verify the result. Planner config fields do not replace these active preferences. Use Check requests and access on the local Access page to refresh approvals. Mark goals_reviewed only after applying the requested goals or the owner explicitly leaves them for later; include it in the final control-enabling review.", "/setup/goals_reviewed", "/site/time_zone", "/batteries", "/vehicles", "/loadpoints", "/v2x")
	}
	if c.Setup == nil || !c.Setup.PriceReviewed {
		add("electricity_price", "Which electricity price area and import/export contract apply here?", "", "", "Read supported countries, zones and currencies from setup_schema. Ask only for missing contract terms. If unknown, continue measuring and show that price planning still needs this information.", "/price", "/setup/price_reviewed")
	}
	if c.Weather == nil || c.Weather.Provider == "" {
		add("weather", "May FTW use this home's location for weather and solar estimates?", "", "latitude/longitude in degrees", "Use a suitable provider from the schema. Ask for a town or coordinates only when the location is not already known; never invent an address.", "/weather")
	}
	base, reason := c.AnnualBaseKWh()
	if reason != "" && (c.ForecastPriors == nil || !c.ForecastPriors.Reviewed && c.ForecastPriors.AnnualBasis != "unknown") {
		add("annual_consumption", "Do you know roughly how much electricity this home uses in a year? You can skip this.", "", "kWh per full year", "Record whether this is total consumption or electricity bought from the grid, whether EV and electric heating are included, and the source and time. Grid purchases alone cannot establish total demand with solar. Remove EV use and separately modelled heating before applying a load prior. Do not invent a split. Read forecast_priors.reason for the missing part; keep earlier answers.", "/forecast_priors")
	}
	if c.MeasurementOnly() {
		add("automatic_control", "After the measurements have been checked, should FTW start automatic control of the equipment in this review?", "automatic_control", "", "First save and measure. Read the live meter identity from commissioning, confirm the exact electrical connection and selected equipment, then validate and review a new draft with measurement_only=false. Save and restart only after the owner confirms. A valid config does not prove an actuation test.", "/setup/measurement_only", "/setup/meter_identity", "/setup/equipment_reviewed")
	}
	source := "generic_prior"
	if c.ForecastPriors != nil && c.ForecastPriors.Source != "" {
		source = c.ForecastPriors.Source
	}
	return map[string]any{
		"questions":         questions,
		"ask_at_most":       3,
		"connection_digest": c.SetupConnectionDigest(),
		"forecast_priors": map[string]any{
			"load_prior_eligible": reason == "", "proposed_base_annual_kwh": base, "reason": reason,
			"source": source, "quality": "cold_start_prior", "uncertainty": "uncalibrated",
			"note": "These answers change only the estimate before sufficient measurements exist. They do not create history, increase sample counts or establish hardware limits. Measured hourly buckets progressively replace the load prior. Unknown PV capacity remains unknown.",
		},
		"resume":       "Answers live in this box's versioned draft. Read setup_status after a pause or client change; retain saved fields and masked credentials. Never reset merely because a reply was lost.",
		"review":       "Show configured equipment, connection, goals, estimates, unresolved items and restart reasons. Confirmation applies only to the exact normalized revision and checksum returned by validation.",
		"everyday_use": "After setup, use status, history, plans and goals directly. Start an interview only when the user asks for setup or changes.",
	}
}

package onboarding

import (
	"reflect"
	"strings"

	"github.com/srcfl/ftw/go/internal/config"
)

// Schema describes this build's complete public configuration type. Driver
// Config maps use the live driver catalogue for their vendor-specific fields.
// Runtime-only fields and stored password hashes never enter this schema.
func Schema() map[string]any {
	schema := schemaFor(reflect.TypeFor[config.Config]())
	schema["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	schema["description"] = "FTW configuration. Patch a draft, then use Core validation. Units are W, Wh, A, V, seconds and SoC fractions 0..1. Arrays replace the complete array; retain entries you did not change. Driver config fields come from /api/drivers/catalog."
	return schema
}

func schemaFor(t reflect.Type) map[string]any {
	if t.Kind() == reflect.Pointer {
		return map[string]any{"anyOf": []any{schemaFor(t.Elem()), map[string]any{"type": "null"}}}
	}
	switch t.Kind() {
	case reflect.Struct:
		properties := map[string]any{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			field := schemaFor(f.Type)
			if description := setupFieldDescription(t, name); description != "" {
				field["description"] = description
			}
			if t == reflect.TypeFor[config.API]() && name == "lan_auth" {
				field["readOnly"] = true
				field["description"] = "Read current house-password protection. Change it through /api/auth/password on the box, not a setup draft."
			}
			properties[name] = field
		}
		return map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	case reflect.Map:
		return map[string]any{"type": []string{"object", "null"}, "additionalProperties": schemaFor(t.Elem())}
	case reflect.Slice:
		return map[string]any{"type": []string{"array", "null"}, "items": schemaFor(t.Elem())}
	case reflect.Array:
		return map[string]any{"type": "array", "items": schemaFor(t.Elem())}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	default:
		return map[string]any{}
	}
}

func setupFieldDescription(t reflect.Type, name string) string {
	if t == reflect.TypeFor[config.Site]() && name == "time_zone" {
		return "The household's IANA time zone, such as Europe/Stockholm. Car goals, the import hour, daily history and savings read this clock. Empty keeps the host's zone, which is UTC in Docker. Core applies a change when it restarts."
	}
	if t == reflect.TypeFor[config.Setup]() {
		return map[string]string{
			"goals_reviewed":       "The owner reviewed operating mode, battery reserve/export, planning style and any car/loadpoint goals. Unknown optional goals can wait.",
			"measurement_only":     "Keep true until equipment has been saved and is measuring. False requests automatic control after exact review; changing this requires a restart.",
			"connection_confirmed": "True only after the owner checks the fuse, phase count and per-phase voltage against the network contract.",
			"connection_digest":    "Exact connection_digest from the current guide after reviewing its fuse and chosen site meter. Changes invalidate the confirmation.",
			"meter_identity":       "Exact live meter_identity from commissioning. Never invent an identity or use a display name.",
			"equipment_reviewed":   "The owner reviewed which equipment belongs to this home and the grid-boundary meter. This does not verify actuation.",
			"price_reviewed":       "The owner has reviewed the price setup, including a choice to leave unknown contract terms for later.",
			"solar_present":        "Owner's answer about installed solar. Unknown is null, never false by assumption.",
		}[name]
	}
	if t == reflect.TypeFor[config.ForecastPriors]() {
		return map[string]string{
			"reviewed":           "True after the optional forecast questions were reviewed, including a choice to skip unknown answers. Retain answers and stop repeating these questions.",
			"annual_kwh":         "Optional electricity use for a full year, kWh. Record its basis and included components; never write historical samples from this answer.",
			"annual_basis":       "total_consumption, grid_import or unknown. Grid import alone cannot establish total demand with solar or storage.",
			"annual_period":      "Period the annual estimate describes, such as 2025 or the last twelve months.",
			"includes_ev":        "Whether annual_kwh includes EV charging. Null means unknown and prevents applying this annual prior.",
			"ev_annual_kwh":      "Annual EV charging kWh included in annual_kwh; subtract before the household baseline because EV is planned separately.",
			"includes_heating":   "Whether annual_kwh includes electric heating. Needed when a separate weather heating term is configured.",
			"heating_annual_kwh": "Annual electric heating kWh included in annual_kwh. Remove it before adding a separately modelled heating term.",
			"heating_type":       "Optional owner's description. Do not turn this into an invented heating coefficient.",
			"occupants":          "Optional household size. Stored context, not an automatic guess about energy use.",
			"source":             "Source of the annual estimate, such as an owner estimate, full-year meter total or electricity bill. A bill may show grid purchases only.",
			"recorded_at":        "RFC3339 time the annual answers were recorded.",
			"solar_source":       "Source of optional PV capacity/geometry estimates. These do not establish inverter control limits.",
			"solar_recorded_at":  "RFC3339 time the solar answers were recorded.",
		}[name]
	}
	return ""
}

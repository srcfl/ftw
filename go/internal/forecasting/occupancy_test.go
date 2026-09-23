package forecasting

import "testing"

func TestOccupancyRequiresBoundedCausalQuarters(t *testing.T) {
	const at int64 = 1781524800000
	good := Occupancy{StartMS: at, EndMS: at + 900000, AvailableAtMS: at, Home: false}
	if err := ValidateOccupancy([]Occupancy{good}, at); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Occupancy){"future": func(r *Occupancy) { r.AvailableAtMS++ }, "unknown availability": func(r *Occupancy) { r.AvailableAtMS = 0 }, "misaligned": func(r *Occupancy) { r.StartMS++; r.EndMS++ }, "wrong duration": func(r *Occupancy) { r.EndMS++ }} {
		t.Run(name, func(t *testing.T) {
			bad := good
			mutate(&bad)
			if ValidateOccupancy([]Occupancy{bad}, at) == nil {
				t.Fatal("invalid occupancy accepted")
			}
		})
	}
	if ValidateOccupancy([]Occupancy{good, good}, at) == nil {
		t.Fatal("overlapping quarters accepted")
	}
	many := make([]Occupancy, 513)
	for i := range many {
		many[i] = good
		many[i].StartMS += int64(i) * 900000
		many[i].EndMS += int64(i) * 900000
	}
	if ValidateOccupancy(many, at) == nil {
		t.Fatal("unbounded horizon accepted")
	}
}

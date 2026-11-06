package api

import "context"

// This separate field must never populate measured.load_wh, the complete
// load_wh total, forecast training or a self-powered percentage.
func (s *Server) addHouseholdSubtotal(ctx context.Context, from, to int64, fields map[string]any) error {
	if fields == nil || fields["load_wh"] != nil {
		return nil
	}
	subtotal, err := s.deps.State.HouseholdEnergySubtotal(ctx, from, to)
	if err == nil && subtotal != nil {
		fields["load_subtotal"] = subtotal
	}
	return err
}

package state

// InvalidateWeatherForecasts discards the replaceable weather cache after a
// source or site change. Immutable issued forecasts remain in the archive.
func (s *Store) InvalidateWeatherForecasts() error {
	_, err := s.cache.Exec("DELETE FROM forecasts")
	return err
}

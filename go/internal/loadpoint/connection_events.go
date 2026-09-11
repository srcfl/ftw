package loadpoint

import "time"

type connectionEdge struct {
	known       bool
	plugged     bool
	unpluggedAt time.Time
}

// Called under the manager lock. Unknown readings never count as unplugging.
// A first reading, including after an outage or restart, only sets a baseline.
func (m *Manager) observeConnectionLocked(id, driver string, plugged bool, now time.Time) bool {
	if m.connectionEdges == nil {
		m.connectionEdges = make(map[string]connectionEdge)
	}
	edge := m.connectionEdges[id]
	if !m.connectionHealth[driver] {
		delete(m.connectionEdges, id)
		return false
	}
	fire := edge.known && !edge.plugged && plugged && now.Sub(edge.unpluggedAt) >= 2*time.Second
	if !plugged && (!edge.known || edge.plugged) {
		edge.unpluggedAt = now
	}
	edge.known, edge.plugged = true, plugged
	m.connectionEdges[id] = edge
	return fire
}

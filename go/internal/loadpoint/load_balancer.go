package loadpoint

import "time"

// MinStartCurrentA is the least current a car charges on: 6 A per phase, the
// lowest pilot current in IEC 61851-1. A charger whose own load balancing
// lets the car take less holds the start back, whatever FTW orders.
const MinStartCurrentA = 6.0

// LoadBalancerRaiseWait bounds how long Core lets a charger's own load
// balancing hold back a start before it reports the start as failed. Easee
// support states that its Equalizer lowers the charging current at once but
// raises it again slowly, up to 15 minutes after household use falls, to
// protect relays and cars (srcfl/ftw#1948, 18 November 2026).
const LoadBalancerRaiseWait = 15 * time.Minute

// LoadBalancerHoldsStart reports whether a driver's limit and its cause say
// that the charger's own load balancing lets the car take too little to start.
func LoadBalancerHoldsStart(limitA *float64, limitedBy string) bool {
	return limitedBy == "load_balancer" && limitA != nil && finite(*limitA) && *limitA >= 0 && *limitA < MinStartCurrentA
}

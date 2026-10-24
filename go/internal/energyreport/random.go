package energyreport

import (
	"crypto/rand"
	"math/big"
)

// randomFloat draws from the system source, as fleetping's does.
func randomFloat() float64 {
	const precision = 1 << 53
	n, err := rand.Int(rand.Reader, big.NewInt(precision))
	if err != nil {
		return 0.5
	}
	return float64(n.Int64()) / precision
}

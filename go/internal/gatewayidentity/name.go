package gatewayidentity

import (
	"crypto/md5" // Used only for the old display-name map, never for security.
	"fmt"
)

// ThreeWordName keeps the Sourceful Energy Gateway display-name map. The input
// is the normalized gateway ID; the output order is adjective-color-animal.
func ThreeWordName(gatewayID string) (string, error) {
	normalized, err := NormalizeGatewayID(gatewayID)
	if err != nil {
		return "", err
	}
	digest := md5.Sum([]byte(normalized))
	indices := [3]byte{
		digest[0] ^ digest[1] ^ digest[2] ^ digest[3] ^ digest[4],
		digest[5] ^ digest[6] ^ digest[7] ^ digest[8] ^ digest[9],
		digest[10] ^ digest[11] ^ digest[12] ^ digest[13] ^ digest[14] ^ digest[15],
	}
	return fmt.Sprintf("%s-%s-%s",
		gatewayAdjectives[indices[0]], gatewayColors[indices[1]], gatewayAnimals[indices[2]]), nil
}

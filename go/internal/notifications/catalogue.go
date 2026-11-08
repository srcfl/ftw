package notifications

import "github.com/srcfl/ftw/go/protocol"

// The push catalogue moved to package protocol so that FTW Cloud can import it
// without the rest of this package. These are the same names under their old
// spelling here.

// PushCatalogueVersion is the catalogue revision these sentences came from.
const PushCatalogueVersion = protocol.PushCatalogueVersion

// Push event kinds.
const (
	PushChargingConnected       = protocol.PushChargingConnected
	PushChargingSessionComplete = protocol.PushChargingSessionComplete
	PushChargingInterrupted     = protocol.PushChargingInterrupted
	PushChargingGoalAtRisk      = protocol.PushChargingGoalAtRisk
	PushUpdateInstalled         = protocol.PushUpdateInstalled
	PushDriverOffline           = protocol.PushDriverOffline
	PushFuseOverLimit           = protocol.PushFuseOverLimit
	PushControlNotFollowing     = protocol.PushControlNotFollowing
	PushBoxUnreachable          = protocol.PushBoxUnreachable
	PushCloudGranted            = protocol.PushCloudGranted
	PushAgentAdded              = protocol.PushAgentAdded
	PushAgentRequest            = protocol.PushAgentRequest
	PushAgentExpiring           = protocol.PushAgentExpiring
	PushApprovalKeySet          = protocol.PushApprovalKeySet
	PushOwnerAdded              = protocol.PushOwnerAdded
	PushCloudRevoked            = protocol.PushCloudRevoked
	PushCarWhich                = protocol.PushCarWhich
)

// PushSentence is one lock-screen sentence, exactly as the app wrote it.
type PushSentence = protocol.PushSentence

// PushSentences is every sentence a push may carry, keyed by kind.
var PushSentences = protocol.PushSentences

// RenderPush fills a catalogue sentence's {arg} placeholders from the event's
// own facts. It refuses to render a sentence it cannot finish.
func RenderPush(kind string, args map[string]string) (title, body string, err error) {
	return protocol.RenderPush(kind, args)
}

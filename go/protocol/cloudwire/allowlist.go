package cloudwire

// Fields is the allowlist of everything the box may send FTW Cloud: one table
// per class, and one per nested shape, as docs/protocol-cloud.md lists them.
// Header holds the four fields every box body carries.
//
// A field reaches a phone only under its class's scope (ClassScope); fields
// of a nested table belong to the class that carries it. Driver names, device
// capabilities and inventory have no row, so the box has no field to put
// them in. TestEveryFieldHasARow fails when a body type grows a field this
// table does not list, or the table lists one the type does not have.
var Fields = map[string][]string{
	"header": {"seq", "boot", "uptimeMs", "wallMs"},

	MsgHello: {"v", "pk", "nonce", "ekm", "boxNonce", "build", "clock", "caps", "rosterSeq", "grantId"},

	MsgLive:  {"fields", "fieldSources", "sources", "srcRev", "ages", "dispatchBlockedBy"},
	"Source": {"kind", "state", "lastOkMs", "staleAfterMs"},

	MsgLP: {"controlRev", "loadpoints", "feedback"},
	"Loadpoint": {
		"id", "src", "plugged", "socPermille", "socSource", "socAgeS",
		"targetPermille", "readyByMs", "recurring", "powerW", "sessionWh",
		"session", "manual", "commandedW", "commandedReason", "surplusOnly",
		"gridDeferred", "planPending", "boost", "nextStartMs", "nextEndMs", "nextWh",
		"carId", "carIdentifiedBy", "carCorrected", "carCorrectedFrom", "choose",
	},
	"Manual":   {"state", "sinceAgeMs", "requestedW", "commandedW", "reason"},
	"Feedback": {"kind", "src", "status", "reason", "severity", "evidence", "fresh", "actualW", "ageMs"},

	MsgCar: {"cars"},
	"Car": {
		"id", "socPermille", "socSource", "socAgeS", "energySinceWh", "socEstimate",
		"chargeLimitPermille", "goal", "surplusOnly",
	},
	"CarGoal": {"targetPermille", "readyByMin", "days", "finishAtLimit"},

	MsgPrices: {"zone", "currency", "t0Ms", "slotMs", "spotMinor", "totalMinor"},

	MsgHB: {"clock", "srcRev", "liveSeq", "lpSeq", "ages"},

	// The app protocol's hist.chunk: power means over 5-minute buckets of
	// the four series the app draws (TileSeries), and nothing else.
	MsgTiles: {"tileId", "etag", "res", "startMs", "stepMs", "series", "data", "partial"},

	MsgEvent: {"eventId", "ts", "kind", "severity", "args", "src", "lp", "cars"},

	MsgRoster: {"rosterSeq", "ttlS", "devices", "grant", "pending", "noise", "webpush"},
	"Device":  {"id", "key", "role", "scopes", "epoch", "tun", "agent"},
	// An agent row's grant (docs/agents.md). The client's name and the
	// ticket came from FTW Cloud's own consent page and support system.
	"Agent":      {"scopes", "ops", "caps", "expiresAtMs", "principal"},
	"Caps":       {"holdS", "maxW", "perMin"},
	"Principal":  {"client", "clientId", "ticket"},
	"Grant":      {"id", "scopes", "purposes", "controller", "termsPolicy", "statsId", "statsPolicy", "statsSinceMs"},
	"Pending":    {"hash", "kind", "ttlS"},
	"WebPushRow": {"id", "dev", "epoch", "endpoint", "ct", "auth"},

	MsgBye: {"why", "purge"},

	// A kind and nothing else: no reading, no id. The one name is the
	// client an owner gave access, on agent.added while the copy carries
	// events, which FTW Cloud's own consent page showed.
	MsgNotice: {"kind", "client"},
	MsgStats:  {"statsId", "policy", "hourMs", "metrics"},

	// tun carries no header and no signature: d is Noise ciphertext its two
	// ends authenticate, and the service reads only dev and sid.
	MsgTun: {"dev", "sid", "d"},
	// x is the same: d is a watch's exchange with the box, and the service
	// reads only dev and id.
	MsgX: {"dev", "id", "d"},
}

// Bodies maps each class the box sends to its body type. Classes the box does
// not send yet (plan, health) have no entry; the service treats their
// bytes as opaque either way. tun is not here: it has no header and is not a
// class, and Unheaded holds it.
var Bodies = map[string]any{
	MsgHello:  Hello{},
	MsgLive:   Live{},
	MsgLP:     LP{},
	MsgCar:    Cars{},
	MsgPrices: Prices{},
	MsgHB:     HB{},
	MsgTiles:  Tiles{},
	MsgEvent:  Event{},
	MsgRoster: Roster{},
	MsgBye:    Bye{},
	MsgNotice: Notice{},
	MsgStats:  Stats{},
}

// Unheaded are the box's messages without the common header.
var Unheaded = map[string]any{
	MsgTun: Tun{},
	MsgX:   X{},
}

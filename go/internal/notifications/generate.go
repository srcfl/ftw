package notifications

// The push sentence table is generated from contract/push-catalogue.yaml —
// Core's public source, copied byte for byte into each client. The app
// owns all prose; push is the stated exception where the box must render
// text, and this is the only place it may take that text from. The table
// lives in package protocol, which FTW Cloud imports too.

//go:generate go run ../appproto/gencontract/cmd push ../../../contract/push-catalogue.yaml ../../protocol/push_gen.go

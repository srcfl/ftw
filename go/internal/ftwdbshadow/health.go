package ftwdbshadow

func (h HealthOps) validate() error {
	if h.SyncPolicy < 1 || h.SyncPolicy > 3 {
		return invalidEnum("sync policy", h.SyncPolicy)
	}
	if (h.SyncPolicy == 3) != (h.SyncEveryBytes > 0) {
		return invalidField("sync every-bytes")
	}
	return nil
}

func encodeHealthOps(out *[]byte, h HealthOps) error {
	if err := h.validate(); err != nil {
		return err
	}
	for _, value := range []uint64{h.OverloadCount, h.ProtocolErrorCount, h.DatabaseBytes, h.DatabasePoints, h.DatabaseCommits, h.RecoveredTailBytes} {
		putUint64(out, value)
	}
	*out = append(*out, h.SyncPolicy)
	putUint64(out, h.SyncEveryBytes)
	putBool(out, h.LastAckDurable)
	return nil
}

func decodeHealthOps(in *input) (*HealthOps, error) {
	h := &HealthOps{}
	for _, value := range []*uint64{&h.OverloadCount, &h.ProtocolErrorCount, &h.DatabaseBytes, &h.DatabasePoints, &h.DatabaseCommits, &h.RecoveredTailBytes} {
		var err error
		*value, err = in.uint64()
		if err != nil {
			return nil, err
		}
	}
	var err error
	if h.SyncPolicy, err = in.byte(); err != nil {
		return nil, err
	}
	if h.SyncEveryBytes, err = in.uint64(); err != nil {
		return nil, err
	}
	if h.LastAckDurable, err = in.boolean("last_ack_durable"); err != nil {
		return nil, err
	}
	return h, h.validate()
}

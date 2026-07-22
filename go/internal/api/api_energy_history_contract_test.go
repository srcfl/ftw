package api

import (
	"testing"

	"github.com/srcfl/ftw/go/internal/homelink"
)

func TestHomeLinkHistoryBoundsMatchAPI(t *testing.T) {
	if energyHistoryMaxWindowMS != homelink.MaxHistoryWindowMS {
		t.Fatalf("history windows differ: api=%d home_link=%d", energyHistoryMaxWindowMS, homelink.MaxHistoryWindowMS)
	}
	if energyHistoryMaxLimit != homelink.MaxHistoryLimit {
		t.Fatalf("history limits differ: api=%d home_link=%d", energyHistoryMaxLimit, homelink.MaxHistoryLimit)
	}
	if energyHistoryMaxBuckets != homelink.MaxHistoryBuckets {
		t.Fatalf("history bucket limits differ: api=%d home_link=%d", energyHistoryMaxBuckets, homelink.MaxHistoryBuckets)
	}
}

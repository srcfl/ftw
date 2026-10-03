package state

import "testing"

func TestLoadConfigByPrefix(t *testing.T) {
	s := freshStore(t)
	if err := s.SaveConfig("driver_secret:old-name:refresh_token", "B"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveConfig("driver_secret:old-name:access_token", "A"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveConfig("other", "x"); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadConfigByPrefix("driver_secret:old-name:")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["driver_secret:old-name:refresh_token"] != "B" || got["driver_secret:old-name:access_token"] != "A" {
		t.Fatalf("prefix rows = %#v", got)
	}
	if _, ok := got["other"]; ok {
		t.Fatal("unrelated key included")
	}
}

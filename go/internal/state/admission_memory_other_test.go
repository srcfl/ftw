//go:build !linux && !darwin

package state

import "testing"

func checkStorageAdmissionRSS(t *testing.T) { t.Skip("RSS admission requires Linux or macOS") }

//go:build !windows

package config

import (
	"fmt"
	"os"
)

func verifyConfigFileOwnerOnly(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if got := info.Mode().Perm(); got != configFileMode.Perm() {
		return fmt.Errorf("saved config mode = %04o, want %04o", got, configFileMode.Perm())
	}
	return nil
}

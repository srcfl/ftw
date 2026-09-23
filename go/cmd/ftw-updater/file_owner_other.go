//go:build !unix

package main

import "os"

// Non-Unix systems have no portable numeric owner to copy. The staged file
// keeps the owner and access rules assigned by the platform.
func preserveFileOwner(_ *os.File, _ os.FileInfo) error { return nil }

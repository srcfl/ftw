package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// explainBindError names the usual reason the HTTP port is taken: FTW is
// already running on this machine, most often as the service.
func explainBindError(addr string, bindErr error) error {
	if !errors.Is(bindErr, syscall.EADDRINUSE) {
		return fmt.Errorf("HTTP port %s could not be opened: %w", addr, bindErr)
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		port = strings.TrimPrefix(addr, ":")
	}
	if !answersAsFTW(port) {
		return fmt.Errorf("port %s is in use by a program that does not answer as FTW; stop it or give FTW another api.port", port)
	}
	check := "ftw status"
	if port != "8080" {
		check += " --url http://127.0.0.1:" + port
	}
	return fmt.Errorf("FTW is already running on this machine at http://127.0.0.1:%s, so this copy did not start. Check it with: %s", port, check)
}

// answersAsFTW asks /api/health, which a Core still opening its state also
// answers.
func answersAsFTW(port string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+port+"/api/health", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var health struct {
		Status string `json:"status"`
	}
	return json.NewDecoder(resp.Body).Decode(&health) == nil && health.Status != ""
}

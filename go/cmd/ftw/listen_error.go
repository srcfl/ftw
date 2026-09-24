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

func explainBindError(addr string, bindErr error) error {
	if bindErr == nil {
		return nil
	}
	if !errors.Is(bindErr, syscall.EADDRINUSE) {
		return fmt.Errorf("HTTP port %s could not be opened: %w", addr, bindErr)
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		port = strings.TrimPrefix(addr, ":")
	}
	if version, ok := probeFTW(port); ok {
		return fmt.Errorf("FTW is already running on this machine at http://127.0.0.1:%s (%s). Another copy was not started. Use ftw status via ftw doctor, or ftw update", port, version)
	}
	return fmt.Errorf("port %s is already in use, and it did not answer as FTW. Choose another port with -port", port)
}

func probeFTW(port string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+port+"/api/version/check", nil)
	if err != nil {
		return "", false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	var info struct {
		Current string `json:"current"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil || info.Current == "" {
		return "", false
	}
	return info.Current, true
}

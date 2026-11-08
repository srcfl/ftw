package main

import (
	"log/slog"
	"time"
)

// notificationLogRetries spaces the tries after a failed notification_log
// write: a long writer, such as the daily snapshot, can hold the database
// for longer than the busy timeout, and the row is the only record of what
// the owner was told.
var notificationLogRetries = []time.Duration{time.Second, 4 * time.Second, 15 * time.Second, time.Minute, 5 * time.Minute}

// recordWithRetry writes now and, if that fails, tries again in the
// background so the event bus never waits on the database.
func recordWithRetry(what string, write func() error) {
	err := write()
	if err == nil {
		return
	}
	go func() {
		for i, wait := range notificationLogRetries {
			time.Sleep(wait)
			if err = write(); err == nil {
				slog.Info(what+": recorded after a retry", "tries", i+2)
				return
			}
		}
		slog.Warn(what+": record failed after retries", "tries", len(notificationLogRetries)+1, "err", err)
	}()
}

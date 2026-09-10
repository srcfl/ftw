package state

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// historyRotateMinRSS is the process RSS at which hourly maintenance reopens
// the native DuckDB instance to drop index buffers. 0 means always rotate
// (tests). Below the threshold, maintenance only checkpoints the WAL.
var historyRotateMinRSS int64 = 768 << 20

func processRSSBytes() (int64, bool) {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, false
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, false
		}
		return kb * 1024, true
	}
	return 0, false
}

func shouldRotateNative() bool {
	if historyRotateMinRSS <= 0 {
		return true
	}
	rss, ok := processRSSBytes()
	return ok && rss >= historyRotateMinRSS
}

package runtime

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// currentRSS reads the current resident set of the bot process, not virtual
// memory, Go heap statistics, peak RSS, or the sum of its worker processes.
func currentRSS() (uint64, error) {
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, err
	}
	return parseRSS(string(status))
}

func parseRSS(status string) (uint64, error) {
	for _, line := range strings.Split(status, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "VmRSS:" {
			continue
		}
		// Linux labels these values kB, but uses 1024-byte units.
		if len(fields) != 3 || fields[2] != "kB" {
			return 0, fmt.Errorf("invalid VmRSS format")
		}
		kib, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil || kib > ^uint64(0)/1024 {
			return 0, fmt.Errorf("invalid VmRSS value")
		}
		return kib * 1024, nil
	}
	return 0, fmt.Errorf("VmRSS unavailable")
}

func formatRSS(bytes uint64, err error) string {
	if err != nil {
		// Do not leak local paths or substitute a different memory metric.
		return "Unavailable"
	}
	return fmt.Sprintf("%.1f MiB", float64(bytes)/(1024*1024))
}

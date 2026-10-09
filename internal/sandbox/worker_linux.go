//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

func becomeRoot() error {
	if os.Getenv("Q_AGENT_WORKER") != "1" || os.Getuid() != 65536 || os.Getgid() != 65536 {
		return fmt.Errorf("worker requires the q-agent user namespace")
	}
	for _, kind := range []string{"uid", "gid"} {
		data, err := os.ReadFile("/proc/self/" + kind + "_map")
		if err != nil {
			return err
		}
		fields := strings.Fields(string(data))
		if len(fields) != 6 || fields[0] != "0" || fields[2] != "65536" || fields[3] != "65536" || fields[5] != "1" {
			return fmt.Errorf("worker mapping validation failed")
		}
		base, e := strconv.Atoi(fields[1])
		if e != nil || base <= 0 {
			return fmt.Errorf("worker mapping validation failed")
		}
	}
	if err := syscall.Setgroups(nil); err != nil && err != syscall.EPERM {
		return err
	}
	if err := syscall.Setgid(0); err != nil {
		return err
	}
	if err := syscall.Setuid(0); err != nil {
		return err
	}
	if os.Getuid() != 0 || os.Getgid() != 0 {
		return fmt.Errorf("worker failed to become namespace root")
	}
	return nil
}

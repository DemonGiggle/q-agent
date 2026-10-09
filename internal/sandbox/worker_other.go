//go:build !linux

package sandbox

import "fmt"

func becomeRoot() error { return fmt.Errorf("worker requires Linux") }

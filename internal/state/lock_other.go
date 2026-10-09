//go:build !linux

package state

import "fmt"

func Lock(string) (func(), error) { return nil, fmt.Errorf("service requires Linux") }

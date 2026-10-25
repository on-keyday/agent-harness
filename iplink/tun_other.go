//go:build !linux

package iplink

import "net/netip"

// OpenTun is Linux-only in stage 1. Windows (wintun) is a later stage.
func OpenTun(addr netip.Prefix, mtu int) (Device, error) { return nil, ErrNotLinux }

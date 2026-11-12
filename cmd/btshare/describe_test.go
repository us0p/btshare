package main

import (
	"errors"
	"testing"

	"github.com/us0p/btshare/internal/bluez"
	"github.com/us0p/btshare/internal/switcher"
)

func TestDescribe(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    switcher.HostResult
		want string
	}{
		{"error", switcher.HostResult{Err: errors.New("down")}, "error: down"},
		{"not paired", switcher.HostResult{}, "not paired"},
		{"connected", switcher.HostResult{State: bluez.DeviceState{Known: true, Connected: true, Trusted: true}}, "CONNECTED"},
		// Release untrusts the device instead of blocking it.
		{"released (untrusted)", switcher.HostResult{State: bluez.DeviceState{Known: true}}, "released"},
		// Older versions blocked it.
		{"released (blocked)", switcher.HostResult{State: bluez.DeviceState{Known: true, Trusted: true, Blocked: true}}, "released"},
		{"free", switcher.HostResult{State: bluez.DeviceState{Known: true, Trusted: true}}, "not connected"},
	} {
		if got := describe(tc.r); got != tc.want {
			t.Errorf("%s: describe = %q, want %q", tc.name, got, tc.want)
		}
	}
}

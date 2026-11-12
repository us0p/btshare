package main

import (
	"errors"
	"testing"
	"time"
)

func TestWaitForAddressSurvivesAdapterReRegistration(t *testing.T) {
	calls := 0
	get := func() (string, error) {
		calls++
		switch {
		case calls <= 2: // adapter object gone while bluetoothd re-registers it
			return "", errors.New(`Method "Get" with signature "ss" on interface "org.freedesktop.DBus.Properties" doesn't exist`)
		case calls == 3: // back, but not yet reporting the new address
			return "00:D7:6D:78:90:4B", nil
		default:
			return "44:01:bb:88:41:91", nil
		}
	}
	got, err := waitForAddress(get, "44:01:BB:88:41:91", 5*time.Second)
	if err != nil || got != "44:01:bb:88:41:91" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestWaitForAddressReportsRefusedChange(t *testing.T) {
	got, err := waitForAddress(func() (string, error) { return "00:D7:6D:78:90:4B", nil },
		"44:01:BB:88:41:91", 300*time.Millisecond)
	if err != nil || got != "00:D7:6D:78:90:4B" {
		t.Fatalf("got %q, %v", got, err)
	}
}

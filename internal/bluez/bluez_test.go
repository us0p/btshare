package bluez

import (
	"errors"
	"fmt"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestInProgress(t *testing.T) {
	pending := dbus.Error{Name: "org.bluez.Error.InProgress", Body: []any{"Operation already in progress"}}
	if !inProgress(pending) || !inProgress(fmt.Errorf("connect: %w", pending)) {
		t.Fatal("InProgress not recognized")
	}
	failed := dbus.Error{Name: "org.bluez.Error.Failed", Body: []any{"Page Timeout"}}
	if inProgress(failed) || inProgress(errors.New("in progress")) || inProgress(nil) {
		t.Fatal("other errors taken for InProgress")
	}
}

// Package notify shows desktop notifications through the freedesktop
// notification service on the session bus (GNOME, KDE, dunst, mako...),
// falling back to notify-send. Identical messages are rate-limited so a
// misbehaving peer can't flood the desktop.
package notify

import (
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	urgencyNormal   byte = 1
	urgencyCritical byte = 2
)

type Notifier struct {
	App    string
	Dedupe time.Duration // suppress identical messages within this window

	mu   sync.Mutex
	conn *dbus.Conn
	last map[string]time.Time
}

func New(app string) *Notifier {
	return &Notifier{App: app, Dedupe: time.Minute}
}

// Error shows a notification that stays visible until dismissed on most desktops.
func (n *Notifier) Error(summary, body string) {
	n.send(summary, body, "dialog-error", urgencyCritical, 0)
}

// Info shows a short-lived notification.
func (n *Notifier) Info(summary, body string) {
	n.send(summary, body, "bluetooth", urgencyNormal, 4000)
}

func (n *Notifier) send(summary, body, icon string, urgency byte, timeoutMS int32) {
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()

	key := summary + "\x00" + body
	if n.last == nil {
		n.last = map[string]time.Time{}
	}
	if t, ok := n.last[key]; ok && time.Since(t) < n.Dedupe {
		return
	}
	n.last[key] = time.Now()

	if n.viaDBus(summary, body, icon, urgency, timeoutMS) == nil {
		return
	}
	// Session bus unreachable (e.g. agent started before login): try the CLI.
	if p, err := exec.LookPath("notify-send"); err == nil {
		u := "normal"
		if urgency == urgencyCritical {
			u = "critical"
		}
		_ = exec.Command(p, "-a", n.App, "-i", icon, "-u", u,
			"-t", strconv.Itoa(int(timeoutMS)), summary, body).Run()
	}
}

func (n *Notifier) viaDBus(summary, body, icon string, urgency byte, timeoutMS int32) error {
	for attempt := 0; attempt < 2; attempt++ {
		if n.conn == nil || !n.conn.Connected() {
			c, err := dbus.ConnectSessionBus()
			if err != nil {
				return err
			}
			n.conn = c
		}
		obj := n.conn.Object("org.freedesktop.Notifications", "/org/freedesktop/Notifications")
		call := obj.Call("org.freedesktop.Notifications.Notify", 0,
			n.App, uint32(0), icon, summary, body, []string{},
			map[string]dbus.Variant{"urgency": dbus.MakeVariant(urgency)},
			timeoutMS)
		if call.Err == nil {
			return nil
		}
		n.conn.Close()
		n.conn = nil
		if attempt == 1 {
			return call.Err
		}
	}
	return nil
}

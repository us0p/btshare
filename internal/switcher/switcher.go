// Package switcher moves a device between hosts, in one of two ways.
//
// Multi-channel devices (config.Device.MultiChannel) are paired with each
// computer on their own channel. The target starts listening, then the
// computer holding the device tells it to change to the target's channel,
// like pressing its channel button. Nothing is released or blocked.
//
// Single-host devices have one pairing that every computer shares. The target starts
// acquiring first (unblocked and waiting for the device), then every other
// host releases it: a BLE device sends only a short burst of reconnection
// advertising right after a disconnect, so the target must already be
// listening when that happens. If the target can't connect, the device is
// handed back to the host that had it the same way. A device holds one
// connection at a time, and hosts that release it block it, so it never
// ends up connected to two hosts.
package switcher

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/us0p/btshare/internal/bluez"
	"github.com/us0p/btshare/internal/config"
	"github.com/us0p/btshare/internal/node"
)

// Timeouts are vars so tests can shrink them.
var (
	QueryTimeout   = 4 * time.Second
	ReleaseTimeout = 6 * time.Second
	AcquireTimeout = 20 * time.Second
	// HeadStart is how long the target gets to unblock the device and start
	// connecting before the other hosts let go of it.
	HeadStart = 500 * time.Millisecond
)

type Switcher struct {
	Order []string             // host names, cycle order for Next
	Nodes map[string]node.Node // by host name
	Self  string
	// Logf, if set, receives a timeline of each switch: who held the
	// device, every release/acquire with its duration and error, and any
	// rollback.
	Logf func(format string, args ...any)
}

func (s *Switcher) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// logResults logs one line per host for a parallel step.
func (s *Switcher) logResults(step string, rs []HostResult, took time.Duration) {
	for _, r := range rs {
		s.logf("  %s on %s: connected=%v blocked=%v known=%v err=%v (step took %s)",
			step, r.Host, r.State.Connected, r.State.Blocked, r.State.Known, r.Err, took)
	}
}

// HostResult is the outcome of an operation on one host.
type HostResult struct {
	Host  string
	State bluez.DeviceState
	Err   error
}

// Report is the outcome of a switch.
type Report struct {
	Target   string
	Released []HostResult // one per non-target host
	Acquired HostResult
	// Restored is set when the target failed to connect and the device was
	// handed back to the host that held it before the switch.
	Restored *HostResult
}

// Status queries every host for the device's state, each at the address
// it uses for the device.
func (s *Switcher) Status(ctx context.Context, d config.Device) []HostResult {
	return s.forEachHost(ctx, QueryTimeout, s.Order, func(c context.Context, h string, n node.Node) (bluez.DeviceState, error) {
		return n.State(c, d.MACOn(h))
	})
}

func (s *Switcher) forEachHost(ctx context.Context, timeout time.Duration, hosts []string,
	op func(context.Context, string, node.Node) (bluez.DeviceState, error)) []HostResult {
	out := make([]HostResult, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			st, err := op(cctx, h, s.Nodes[h])
			out[i] = HostResult{Host: h, State: st, Err: err}
		}()
	}
	wg.Wait()
	return out
}

// Switch moves the device to target.
//
// If the target can't connect, the device is released there again and
// reconnected to the host that held it before (see Report.Restored), so a
// failed switch doesn't leave it blocked everywhere.
//
// Release failures on other hosts are reported but don't abort: an
// unreachable host is usually powered off and therefore can't be holding
// the device. If a host that still reports the device as connected couldn't
// release it, we abort instead of fighting over it.
func (s *Switcher) Switch(ctx context.Context, d config.Device, target string) (*Report, error) {
	t := time.Now()
	results := s.Status(ctx, d)
	s.logResults("status", results, since(t))
	holder, _ := Holder(results)
	return s.switchFrom(ctx, d, target, holder)
}

// switchFrom is Switch with the current holder ("" if none) already known.
func (s *Switcher) switchFrom(ctx context.Context, d config.Device, target, holder string) (*Report, error) {
	if _, ok := s.Nodes[target]; !ok {
		return nil, fmt.Errorf("unknown host %q", target)
	}
	if d.MultiChannel() {
		return s.switchChannel(ctx, d, target, holder)
	}
	mac := d.MAC
	var others []string
	for _, h := range s.Order {
		if h != target {
			others = append(others, h)
		}
	}
	s.logf("switch %s: %q -> %q (hosts %v)", mac, holder, target, s.Order)
	rep := &Report{Target: target}
	var err error
	rep.Released, rep.Acquired, err = s.handoff(ctx, mac, target, others)
	if err != nil {
		return rep, err
	}
	if err := rep.Acquired.Err; err != nil {
		err = fmt.Errorf("%s could not connect: %w", target, err)
		if holder == "" || holder == target {
			return rep, err
		}
		return rep, s.restore(ctx, rep, mac, target, holder, err)
	}
	return rep, nil
}

// restore hands the device back to holder after target failed to connect.
// It runs even if ctx was canceled (e.g. Ctrl+C mid-switch): stopping here
// would leave the device blocked on every host.
func (s *Switcher) restore(ctx context.Context, rep *Report, mac, target, holder string, cause error) error {
	ctx = context.WithoutCancel(ctx)
	// Same handoff in reverse: the holder starts acquiring, then the target
	// is blocked again so it can't grab the device late.
	s.logf("switch %s: rolling back to %s", mac, holder)
	_, acquired, aerr := s.handoff(ctx, mac, holder, []string{target})
	rep.Restored = &acquired
	err := acquired.Err
	if aerr != nil {
		err = aerr
	}
	if err != nil {
		return fmt.Errorf("%w; giving it back to %s also failed: %w (run `btshare here` on %s)", cause, holder, err, holder)
	}
	return fmt.Errorf("%w; it is back on %s", cause, holder)
}

// handoff moves the device to target: target starts acquiring, and after
// HeadStart the hosts in from release it in parallel. If a host that still
// has the device connected fails to release it, the acquire is canceled,
// target is blocked again and a non-nil abort error is returned.
func (s *Switcher) handoff(ctx context.Context, mac, target string, from []string) (released []HostResult, acquired HostResult, abort error) {
	actx, cancel := context.WithTimeout(ctx, AcquireTimeout)
	defer cancel()
	t := time.Now()
	done := make(chan HostResult, 1)
	go func() {
		st, err := s.Nodes[target].Acquire(actx, mac)
		done <- HostResult{Host: target, State: st, Err: err}
	}()

	// Give the target a head start, unless it already finished (e.g. it
	// failed at once, or the device was already connected there).
	select {
	case r := <-done:
		done <- r
	case <-time.After(HeadStart):
	case <-ctx.Done():
	}

	rt := time.Now()
	released = s.forEachHost(ctx, ReleaseTimeout, from, func(c context.Context, _ string, n node.Node) (bluez.DeviceState, error) {
		return n.Release(c, mac)
	})
	s.logResults("release", released, since(rt))
	for _, r := range released {
		if r.Err != nil && r.State.Connected {
			s.logf("switch %s: aborted, %s still holds it", mac, r.Host)
			cancel()
			<-done
			// Undo the target's unblock so it doesn't grab the device later.
			rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), ReleaseTimeout)
			_, _ = s.Nodes[target].Release(rctx, mac)
			rcancel()
			return released, HostResult{Host: target}, fmt.Errorf("%s still holds the device and failed to release it: %w", r.Host, r.Err)
		}
	}
	acquired = <-done
	s.logResults("acquire", []HostResult{acquired}, since(t))
	return released, acquired, nil
}

// switchChannel moves a multi-channel device: target starts connecting, and
// after HeadStart the holder tells the device to change to target's channel.
// The device then goes to target by itself. If target still can't connect,
// nothing can call the device back (no computer is connected to it), so the
// error says which button to press.
func (s *Switcher) switchChannel(ctx context.Context, d config.Device, target, holder string) (*Report, error) {
	tgt, ok := d.On(target)
	if !ok {
		return nil, fmt.Errorf("%s isn't set up on %s yet: on %s, pair it on a free channel and run `btshare device add %s <MAC|paired name>`",
			d.Name, target, target, d.Name)
	}
	s.logf("switch %s: %q -> %q (channel %d)", d.Name, holder, target, tgt.Channel)
	rep := &Report{Target: target}
	actx, cancel := context.WithTimeout(ctx, AcquireTimeout)
	defer cancel()
	t := time.Now()
	done := make(chan HostResult, 1)
	go func() {
		st, err := s.Nodes[target].Acquire(actx, tgt.MAC)
		done <- HostResult{Host: target, State: st, Err: err}
	}()

	if holder != "" && holder != target {
		select {
		case r := <-done:
			done <- r
		case <-time.After(HeadStart):
		case <-ctx.Done():
		}
		cctx, ccancel := context.WithTimeout(ctx, ReleaseTimeout)
		ct := time.Now()
		err := s.Nodes[holder].SetChannel(cctx, d.MACOn(holder), tgt.Channel)
		ccancel()
		s.logf("  channel %d via %s: err=%v (took %s)", tgt.Channel, holder, err, since(ct))
		rep.Released = []HostResult{{Host: holder, State: bluez.DeviceState{Connected: err != nil}, Err: err}}
		if err != nil {
			cancel()
			<-done
			return rep, fmt.Errorf("%s could not switch %s to channel %d: %w", holder, d.Name, tgt.Channel, err)
		}
	}
	rep.Acquired = <-done
	s.logResults("acquire", []HostResult{rep.Acquired}, since(t))
	if err := rep.Acquired.Err; err != nil {
		switch holder {
		case "":
			return rep, fmt.Errorf("%s could not connect: %w (no computer has %s connected, so btshare can't change its channel: press its channel button to select channel %d)",
				target, err, d.Name, tgt.Channel)
		case target:
			return rep, fmt.Errorf("%s could not connect: %w", target, err)
		default:
			back := ""
			if h, ok := d.On(holder); ok {
				back = fmt.Sprintf(" (channel %d)", h.Channel)
			}
			return rep, fmt.Errorf("%s did not connect on channel %d: %w; press %s's channel button to go back to %s%s",
				target, tgt.Channel, err, d.Name, holder, back)
		}
	}
	return rep, nil
}

func since(t time.Time) time.Duration { return time.Since(t).Round(10 * time.Millisecond) }

// ErrNoHolder is returned by Holder when no host has the device connected.
var ErrNoHolder = errors.New("no host currently has the device connected")

// Holder returns the host currently connected to the device.
func Holder(results []HostResult) (string, error) {
	for _, r := range results {
		if r.Err == nil && r.State.Connected {
			return r.Host, nil
		}
	}
	return "", ErrNoHolder
}

// NextTarget picks the host after the current holder in cycle order,
// skipping hosts that are unreachable, don't know the device, or (for a
// multi-channel device) have no channel. With no holder it starts from Self.
func (s *Switcher) NextTarget(d config.Device, results []HostResult) (string, error) {
	from, err := Holder(results)
	if err != nil {
		from = s.Self
	}
	usable := map[string]bool{}
	for _, r := range results {
		_, hasChannel := d.On(r.Host)
		usable[r.Host] = r.Err == nil && r.State.Known && (hasChannel || !d.MultiChannel())
	}
	start := 0
	for i, h := range s.Order {
		if h == from {
			start = i
		}
	}
	for k := 1; k <= len(s.Order); k++ {
		h := s.Order[(start+k)%len(s.Order)]
		if h != from && usable[h] {
			return h, nil
		}
	}
	return "", errors.New("no other reachable host has this device paired")
}

// Next moves the device to the next host in cycle order.
func (s *Switcher) Next(ctx context.Context, d config.Device) (*Report, error) {
	t := time.Now()
	results := s.Status(ctx, d)
	s.logResults("status", results, since(t))
	target, err := s.NextTarget(d, results)
	if err != nil {
		s.logf("next %s: %v", d.Name, err)
		return nil, err
	}
	holder, _ := Holder(results)
	return s.switchFrom(ctx, d, target, holder)
}

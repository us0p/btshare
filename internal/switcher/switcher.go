// Package switcher moves a device between hosts: release everywhere except
// the target, then acquire on the target. If the target can't connect, the
// device is handed back to the host that had it. The device is never
// deliberately connected to two hosts at once.
package switcher

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/us0p/btshare/internal/bluez"
	"github.com/us0p/btshare/internal/node"
)

// Timeouts are vars so tests can shrink them.
var (
	QueryTimeout   = 4 * time.Second
	ReleaseTimeout = 6 * time.Second
	AcquireTimeout = 20 * time.Second
)

type Switcher struct {
	Order []string             // host names, cycle order for Next
	Nodes map[string]node.Node // by host name
	Self  string
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

func (s *Switcher) forEach(ctx context.Context, timeout time.Duration, hosts []string,
	op func(context.Context, node.Node) (bluez.DeviceState, error)) []HostResult {
	out := make([]HostResult, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			st, err := op(cctx, s.Nodes[h])
			out[i] = HostResult{Host: h, State: st, Err: err}
		}()
	}
	wg.Wait()
	return out
}

// Status queries every host for the device's state.
func (s *Switcher) Status(ctx context.Context, mac string) []HostResult {
	return s.forEach(ctx, QueryTimeout, s.Order, func(c context.Context, n node.Node) (bluez.DeviceState, error) {
		return n.State(c, mac)
	})
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
func (s *Switcher) Switch(ctx context.Context, mac, target string) (*Report, error) {
	holder, _ := Holder(s.Status(ctx, mac))
	return s.switchFrom(ctx, mac, target, holder)
}

// switchFrom is Switch with the current holder ("" if none) already known.
func (s *Switcher) switchFrom(ctx context.Context, mac, target, holder string) (*Report, error) {
	if _, ok := s.Nodes[target]; !ok {
		return nil, fmt.Errorf("unknown host %q", target)
	}
	var others []string
	for _, h := range s.Order {
		if h != target {
			others = append(others, h)
		}
	}
	rep := &Report{Target: target}
	rep.Released = s.forEach(ctx, ReleaseTimeout, others, func(c context.Context, n node.Node) (bluez.DeviceState, error) {
		return n.Release(c, mac)
	})
	for _, r := range rep.Released {
		if r.Err != nil && r.State.Connected {
			return rep, fmt.Errorf("%s still holds the device and failed to release it: %w", r.Host, r.Err)
		}
	}

	actx, cancel := context.WithTimeout(ctx, AcquireTimeout)
	defer cancel()
	st, err := s.Nodes[target].Acquire(actx, mac)
	rep.Acquired = HostResult{Host: target, State: st, Err: err}
	if err != nil {
		err = fmt.Errorf("%s could not connect: %w", target, err)
		if holder == "" || holder == target {
			return rep, err
		}
		return rep, s.restore(ctx, rep, mac, target, holder, err)
	}
	return rep, nil
}

// restore hands the device back to holder after target failed to connect.
// It runs even if ctx was cancelled (e.g. Ctrl+C mid-switch): stopping here
// would leave the device blocked on every host.
func (s *Switcher) restore(ctx context.Context, rep *Report, mac, target, holder string, cause error) error {
	ctx = context.WithoutCancel(ctx)
	// Block it on the target first: it may still connect there late, and the
	// device must not end up on two hosts.
	rctx, cancel := context.WithTimeout(ctx, ReleaseTimeout)
	_, _ = s.Nodes[target].Release(rctx, mac)
	cancel()

	actx, cancel := context.WithTimeout(ctx, AcquireTimeout)
	defer cancel()
	st, err := s.Nodes[holder].Acquire(actx, mac)
	rep.Restored = &HostResult{Host: holder, State: st, Err: err}
	if err != nil {
		return fmt.Errorf("%w; giving it back to %s also failed: %v (run `btshare here` on %s)", cause, holder, err, holder)
	}
	return fmt.Errorf("%w; it is back on %s", cause, holder)
}

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
// skipping hosts that are unreachable or don't know the device. With no
// holder it starts from Self.
func (s *Switcher) NextTarget(results []HostResult) (string, error) {
	from, err := Holder(results)
	if err != nil {
		from = s.Self
	}
	usable := map[string]bool{}
	for _, r := range results {
		usable[r.Host] = r.Err == nil && r.State.Known
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
func (s *Switcher) Next(ctx context.Context, mac string) (*Report, error) {
	results := s.Status(ctx, mac)
	target, err := s.NextTarget(results)
	if err != nil {
		return nil, err
	}
	holder, _ := Holder(results)
	return s.switchFrom(ctx, mac, target, holder)
}

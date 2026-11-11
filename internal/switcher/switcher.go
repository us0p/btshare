// Package switcher moves a device between hosts: release everywhere except
// the target, then acquire on the target. The device is never deliberately
// connected to two hosts at once.
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
// Release failures on other hosts are reported but don't abort: an
// unreachable host is usually powered off and therefore can't be holding
// the device. If a host that still reports the device as connected couldn't
// release it, we abort instead of fighting over it.
func (s *Switcher) Switch(ctx context.Context, mac, target string) (*Report, error) {
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
		return rep, fmt.Errorf("%s could not connect: %w", target, err)
	}
	return rep, nil
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
	target, err := s.NextTarget(s.Status(ctx, mac))
	if err != nil {
		return nil, err
	}
	return s.Switch(ctx, mac, target)
}

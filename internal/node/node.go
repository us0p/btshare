// Package node abstracts "a host that can hold a device": either this machine
// (talking to BlueZ directly) or a peer reached through its agent.
package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/us0p/btshare/internal/auth"
	"github.com/us0p/btshare/internal/bluez"
	"github.com/us0p/btshare/internal/config"
	"github.com/us0p/btshare/internal/hidpp"
)

type Node interface {
	State(ctx context.Context, mac string) (bluez.DeviceState, error)
	Release(ctx context.Context, mac string) (bluez.DeviceState, error)
	Acquire(ctx context.Context, mac string) (bluez.DeviceState, error)
	// SetChannel tells a multi-channel device connected to this host to
	// switch to channel (1-based), handing it to the computer on that channel.
	SetChannel(ctx context.Context, mac string, channel int) error
}

// Request is the JSON body for every agent endpoint.
type Request struct {
	MAC     string `json:"mac"`
	Channel int    `json:"channel,omitempty"` // for /v1/channel
}

// Response is the JSON body every agent endpoint returns.
type Response struct {
	State bluez.DeviceState `json:"state"`
	Error string            `json:"error,omitempty"`
}

// JoinRequest announces a host (and the devices it knows) to a peer.
type JoinRequest struct {
	Host    config.Host     `json:"host"`
	Devices []config.Device `json:"devices"`
}

// JoinResponse returns the peer's view of the group, so the joiner can
// append hosts and devices it doesn't know yet.
type JoinResponse struct {
	Hosts   []config.Host   `json:"hosts"`
	Devices []config.Device `json:"devices"`
	Error   string          `json:"error,omitempty"`
}

// DeviceRequest adds a device to a peer's config.
type DeviceRequest struct {
	Device config.Device `json:"device"`
}

// ErrorResponse is returned by endpoints with nothing else to say.
type ErrorResponse struct {
	Error string `json:"error,omitempty"`
}

// ---- local ----

// Local drives this host's adapter. Changes to one device are serialized,
// so a local CLI call and agent requests can't interleave a release with an
// acquire of it. Different devices don't wait for each other (a 15s connect
// of one mouse mustn't hold up another), and State never waits: it only
// reads, and a status query stuck behind a connect times out on the caller.
type Local struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex // by MAC
	bz    *bluez.Client
}

// lock locks mac's device and returns the unlock function.
func (l *Local) lock(mac string) func() {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = map[string]*sync.Mutex{}
	}
	m := l.locks[strings.ToUpper(mac)]
	if m == nil {
		m = &sync.Mutex{}
		l.locks[strings.ToUpper(mac)] = m
	}
	l.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// NewLocal opens this host's adapter. logf, if not nil, receives a detailed
// trace of every release and acquire (see bluez.Client.Logf).
func NewLocal(adapter string, logf func(format string, args ...any)) (*Local, error) {
	bz, err := bluez.New(adapter)
	if err != nil {
		return nil, err
	}
	bz.Logf = logf
	return &Local{bz: bz}, nil
}

func (l *Local) State(_ context.Context, mac string) (bluez.DeviceState, error) {
	return l.bz.State(mac)
}

func (l *Local) Release(_ context.Context, mac string) (bluez.DeviceState, error) {
	defer l.lock(mac)()
	return l.bz.Release(mac)
}

func (l *Local) Acquire(ctx context.Context, mac string) (bluez.DeviceState, error) {
	defer l.lock(mac)()
	return l.bz.Acquire(ctx, mac)
}

// Unblock lets the device connect to this host again (see bluez.Client.Unblock).
func (l *Local) Unblock(_ context.Context, mac string) (bluez.DeviceState, error) {
	defer l.lock(mac)()
	return l.bz.Unblock(mac)
}

func (l *Local) SetChannel(_ context.Context, mac string, channel int) error {
	defer l.lock(mac)()
	if l.bz.Logf != nil {
		l.bz.Logf("channel %s: switching it to channel %d", mac, channel)
	}
	err := hidpp.SetChannel(mac, channel)
	if err != nil && l.bz.Logf != nil {
		l.bz.Logf("channel %s: %v", mac, err)
	}
	return err
}

// ---- remote ----

type Remote struct {
	Addr   string // host:port
	Secret []byte
	HTTP   *http.Client
}

// post sends a signed JSON request and decodes the reply into out. A
// non-empty "error" field in the reply is returned as an error.
func (r *Remote) post(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+r.Addr+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	auth.Sign(req, r.Secret, body, time.Now())

	hc := r.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var e ErrorResponse
	if err := json.Unmarshal(raw, &e); err != nil {
		return fmt.Errorf("agent %s: HTTP %d: %s", r.Addr, resp.StatusCode, bytes.TrimSpace(raw))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("agent %s: %w", r.Addr, err)
		}
	}
	if e.Error != "" {
		return fmt.Errorf("%s", e.Error)
	}
	return nil
}

func (r *Remote) call(ctx context.Context, op, mac string) (bluez.DeviceState, error) {
	var out Response
	err := r.post(ctx, "/v1/"+op, Request{MAC: mac}, &out)
	return out.State, err
}

func (r *Remote) Join(ctx context.Context, req JoinRequest) (JoinResponse, error) {
	var out JoinResponse
	err := r.post(ctx, "/v1/join", req, &out)
	return out, err
}

func (r *Remote) AddDevice(ctx context.Context, d config.Device) error {
	return r.post(ctx, "/v1/device", DeviceRequest{Device: d}, nil)
}

// RemoveDevice asks the peer to drop the device (matched by MAC) from its config.
func (r *Remote) RemoveDevice(ctx context.Context, d config.Device) error {
	return r.post(ctx, "/v1/device/remove", DeviceRequest{Device: d}, nil)
}

func (r *Remote) SetChannel(ctx context.Context, mac string, channel int) error {
	return r.post(ctx, "/v1/channel", Request{MAC: mac, Channel: channel}, nil)
}

func (r *Remote) State(ctx context.Context, mac string) (bluez.DeviceState, error) {
	return r.call(ctx, "state", mac)
}
func (r *Remote) Release(ctx context.Context, mac string) (bluez.DeviceState, error) {
	return r.call(ctx, "release", mac)
}
func (r *Remote) Acquire(ctx context.Context, mac string) (bluez.DeviceState, error) {
	return r.call(ctx, "acquire", mac)
}

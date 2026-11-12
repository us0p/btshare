// Package agent serves this host's Node over HTTP to authenticated peers,
// and accepts group membership updates (join / device add) from them.
// Failures are surfaced as desktop notifications.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/us0p/btshare/internal/auth"
	"github.com/us0p/btshare/internal/bluez"
	"github.com/us0p/btshare/internal/config"
	"github.com/us0p/btshare/internal/node"
)

// AcquireTimeout bounds how long a peer may keep us retrying Connect.
const AcquireTimeout = 15 * time.Second

// Notifier shows messages to the user on the desktop.
type Notifier interface {
	Error(summary, body string)
	Info(summary, body string)
}

type Server struct {
	Node node.Node
	// Secret is used when the config can't be read; otherwise the config's
	// current secret is, so `btshare join` or `reconfigure` changing it
	// takes effect without restarting the agent.
	Secret     []byte
	ConfigPath string // re-read on every request so joins/device adds apply live
	Notify     Notifier
	Log        *log.Logger
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/state", s.authed(s.deviceOp("check", s.Node.State)))
	mux.HandleFunc("POST /v1/release", s.authed(s.deviceOp("release", s.Node.Release)))
	mux.HandleFunc("POST /v1/acquire", s.authed(s.deviceOp("connect", func(ctx context.Context, mac string) (bluez.DeviceState, error) {
		ctx, cancel := context.WithTimeout(ctx, AcquireTimeout)
		defer cancel()
		return s.Node.Acquire(ctx, mac)
	})))
	mux.HandleFunc("POST /v1/channel", s.authed(s.setChannel))
	mux.HandleFunc("POST /v1/join", s.authed(s.join))
	mux.HandleFunc("POST /v1/device", s.authed(s.addDevice))
	mux.HandleFunc("POST /v1/device/remove", s.authed(s.removeDevice))
	return mux
}

type handler func(w http.ResponseWriter, r *http.Request, body []byte)

// authed reads the body and verifies the HMAC before calling h.
func (s *Server) authed(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, node.ErrorResponse{Error: "read body: " + err.Error()})
			return
		}
		if err := auth.Verify(r, s.secret(), body, time.Now()); err != nil {
			s.logf("rejected %s from %s: %v", r.URL.Path, r.RemoteAddr, err)
			s.notifyError("Rejected request from "+remoteIP(r),
				fmt.Sprintf("%v. If this is one of your computers, its btshare secret or clock is wrong.", err))
			writeJSON(w, http.StatusUnauthorized, node.ErrorResponse{Error: "unauthorized: " + err.Error()})
			return
		}
		h(w, r, body)
	}
}

func (s *Server) secret() []byte {
	if s.ConfigPath != "" {
		if c, err := config.Load(s.ConfigPath); err == nil {
			return []byte(c.Secret)
		}
	}
	return s.Secret
}

func (s *Server) loadConfig() (*config.Config, error) {
	c, err := config.Load(s.ConfigPath)
	if err != nil {
		s.notifyError("Configuration error", err.Error())
	}
	return c, err
}

func (s *Server) deviceOp(verb string, op func(context.Context, string) (bluez.DeviceState, error)) handler {
	return func(w http.ResponseWriter, r *http.Request, body []byte) {
		var req node.Request
		if err := json.Unmarshal(body, &req); err != nil || req.MAC == "" {
			writeJSON(w, http.StatusBadRequest, node.Response{Error: `body must be {"mac": ...}`})
			return
		}
		cfg, err := s.loadConfig()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, node.Response{Error: err.Error()})
			return
		}
		dev, err := cfg.Device(req.MAC)
		if err != nil {
			s.notifyError("Unknown device requested by "+remoteIP(r),
				fmt.Sprintf("%s is not in this computer's config. Run `btshare device add` to share it.", req.MAC))
			writeJSON(w, http.StatusForbidden, node.Response{Error: "device " + req.MAC + " is not in " + cfg.Self + "'s config"})
			return
		}
		st, err := op(r.Context(), req.MAC)
		resp := node.Response{State: st}
		code := http.StatusOK
		if err != nil {
			resp.Error = err.Error()
			code = http.StatusConflict
			s.notifyError(fmt.Sprintf("Could not %s %s", verb, dev.Name), err.Error())
		}
		s.logf("%s %s from %s -> connected=%v blocked=%v err=%v",
			r.URL.Path, dev.Name, r.RemoteAddr, st.Connected, st.Blocked, err)
		writeJSON(w, code, resp)
	}
}

// setChannel switches a multi-channel device connected here to another
// computer's channel.
func (s *Server) setChannel(w http.ResponseWriter, r *http.Request, body []byte) {
	var req node.Request
	if err := json.Unmarshal(body, &req); err != nil || req.MAC == "" || req.Channel < 1 {
		writeJSON(w, http.StatusBadRequest, node.ErrorResponse{Error: `body must be {"mac": ..., "channel": N}`})
		return
	}
	cfg, err := s.loadConfig()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, node.ErrorResponse{Error: err.Error()})
		return
	}
	dev, err := cfg.Device(req.MAC)
	if err != nil {
		writeJSON(w, http.StatusForbidden, node.ErrorResponse{Error: "device " + req.MAC + " is not in " + cfg.Self + "'s config"})
		return
	}
	err = s.Node.SetChannel(r.Context(), req.MAC, req.Channel)
	s.logf("%s %s channel %d from %s -> err=%v", r.URL.Path, dev.Name, req.Channel, r.RemoteAddr, err)
	if err != nil {
		s.notifyError(fmt.Sprintf("Could not switch %s to channel %d", dev.Name, req.Channel), err.Error())
		writeJSON(w, http.StatusConflict, node.ErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, node.ErrorResponse{})
}

func (s *Server) join(w http.ResponseWriter, r *http.Request, body []byte) {
	var req node.JoinRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, node.JoinResponse{Error: "bad join request"})
		return
	}
	var isNew bool
	cfg, err := config.Update(s.ConfigPath, func(c *config.Config) error {
		if req.Host.Name == c.Self {
			return fmt.Errorf("host name %q is already used by the computer you're joining; reinstall with --name <other>", req.Host.Name)
		}
		_, notFound := c.Host(req.Host.Name)
		isNew = notFound != nil
		c.UpsertHost(req.Host)
		for _, d := range req.Devices {
			c.UpsertDevice(d)
		}
		return nil
	})
	if err != nil {
		s.notifyError("Failed to add computer "+req.Host.Name, err.Error())
		writeJSON(w, http.StatusConflict, node.JoinResponse{Error: err.Error()})
		return
	}
	s.logf("join: %s at %s (%d devices)", req.Host.Name, req.Host.Addr, len(req.Devices))
	if isNew && s.Notify != nil {
		s.Notify.Info("Computer added", fmt.Sprintf("%s (%s) can now share Bluetooth devices with this computer.", req.Host.Name, req.Host.Addr))
	}
	writeJSON(w, http.StatusOK, node.JoinResponse{Hosts: cfg.Hosts, Devices: cfg.Devices})
}

func (s *Server) addDevice(w http.ResponseWriter, r *http.Request, body []byte) {
	var req node.DeviceRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, node.ErrorResponse{Error: "bad device request"})
		return
	}
	var changed bool
	_, err := config.Update(s.ConfigPath, func(c *config.Config) error {
		changed = c.UpsertDevice(req.Device)
		return nil
	})
	if err != nil {
		s.notifyError("Failed to add device "+req.Device.Name, err.Error())
		writeJSON(w, http.StatusConflict, node.ErrorResponse{Error: err.Error()})
		return
	}
	s.logf("device: %s %s from %s (changed=%v)", req.Device.Name, req.Device.MAC, r.RemoteAddr, changed)
	writeJSON(w, http.StatusOK, node.ErrorResponse{})
}

func (s *Server) removeDevice(w http.ResponseWriter, r *http.Request, body []byte) {
	var req node.DeviceRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Device.MAC == "" {
		writeJSON(w, http.StatusBadRequest, node.ErrorResponse{Error: "bad device request"})
		return
	}
	var removed bool
	_, err := config.Update(s.ConfigPath, func(c *config.Config) error {
		removed = c.RemoveDevice(req.Device.MAC)
		return nil
	})
	if err != nil {
		s.notifyError("Failed to remove device "+req.Device.Name, err.Error())
		writeJSON(w, http.StatusConflict, node.ErrorResponse{Error: err.Error()})
		return
	}
	s.logf("device removed: %s %s from %s (was present=%v)", req.Device.Name, req.Device.MAC, r.RemoteAddr, removed)
	// btshare no longer manages it, so don't leave it untrusted (or blocked)
	// here by an earlier release (multi-channel devices are never released).
	if u, ok := s.Node.(unblocker); ok && removed && !req.Device.MultiChannel() {
		st, err := u.Unblock(r.Context(), req.Device.MAC)
		s.logf("device removed: unblock %s -> blocked=%v err=%v", req.Device.MAC, st.Blocked, err)
	}
	writeJSON(w, http.StatusOK, node.ErrorResponse{})
}

// unblocker is implemented by node.Local.
type unblocker interface {
	Unblock(ctx context.Context, mac string) (bluez.DeviceState, error)
}

func (s *Server) notifyError(summary, body string) {
	if s.Notify != nil {
		s.Notify.Error("btshare: "+summary, body)
	}
}

func (s *Server) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log.Printf(format, args...)
	}
}

func remoteIP(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

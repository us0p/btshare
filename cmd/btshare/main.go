// Command btshare moves a Bluetooth device (mouse, keyboard, headset...)
// between Linux hosts on the same LAN, so it is only connected to one at a time.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/syslog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/us0p/btshare/internal/agent"
	"github.com/us0p/btshare/internal/bluez"
	"github.com/us0p/btshare/internal/config"
	"github.com/us0p/btshare/internal/group"
	"github.com/us0p/btshare/internal/hidpp"
	"github.com/us0p/btshare/internal/keys"
	"github.com/us0p/btshare/internal/netinfo"
	"github.com/us0p/btshare/internal/node"
	"github.com/us0p/btshare/internal/notify"
	"github.com/us0p/btshare/internal/switcher"
)

const usage = `btshare - share a Bluetooth device between Linux hosts

Setup (install.sh runs init/join for you):
  btshare init [-name N] [-addr IP:PORT] [-secret S]  create config for this host
  btshare reconfigure [-name N] [-port P] [-secret S]  change an existing config
  btshare join <ip[:port]> [-secret S] join the group of the computer at ip;
                                      asks for its secret (see invite)
  btshare invite                      print the command to add another computer
  btshare host add <name> <ip[:port]> append a host manually (no network)
  btshare device add <name> <MAC|paired name>  share a device with all hosts;
                                      run it on every computer for a device
                                      with channels (Logitech Easy-Switch)
  btshare device remove <name|MAC>    stop sharing a device, on all hosts
  btshare device list                 devices paired with this computer

Use:
  btshare agent                       run the LAN agent (one per host)
  btshare status [device]             show which host holds each device
  btshare switch <device> <host>      move device to host
  btshare next   [device]             move device(s) to the next host (cycle)
  btshare here   [device]             move device(s) to this host
                                      (no device: every shared device)
  btshare keys export <device|MAC> [-o file]     (root) export pairing keys
  btshare keys import <file> [-set-address] [-no-restart]  (root) install them
  btshare gen-secret                  print a random shared secret
  btshare version                     print the installed version

Global flags (before the command):
  -v              print a detailed switch/Bluetooth trace (also always in
                  the journal: journalctl -t btshare)
  -config path    config file (default $BTSHARE_CONFIG or ~/.config/btshare/config.toml)
  -notify         also report the result with notify-send (for hotkeys)
`

// version is set at build time by install.sh (-ldflags "-X main.version=...").
var version = "dev"

var (
	configPath = flag.String("config", config.DefaultPath(), "config file")
	notifyFlag = flag.Bool("notify", false, "send a desktop notification with the result")
	verbose    = flag.Bool("v", false, "print the switch/Bluetooth trace to stderr (it always goes to the journal)")
)

// debugLog returns the trace logger for CLI commands. The trace always goes
// to the journal under the "btshare" tag (journalctl -t btshare), so switches
// started from the keyboard shortcut can be investigated later; -v also
// prints it to stderr.
func debugLog() func(format string, args ...any) {
	sl, _ := syslog.New(syslog.LOG_INFO|syslog.LOG_USER, "btshare")
	return func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		if sl != nil {
			_ = sl.Info(msg)
		}
		if *verbose {
			fmt.Fprintf(os.Stderr, "%s %s\n", time.Now().Format("15:04:05.000"), msg)
		}
	}
}

func main() {
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(args); err != nil {
		fmt.Fprintln(os.Stderr, "btshare:", err)
		desktopNotify("btshare failed", err.Error())
		os.Exit(1)
	}
}

// run executes one command; main turns its error into the exit status.
func run(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd, rest := args[0], args[1:]; cmd {
	case "agent":
		err = runAgent(ctx)
	case "init":
		err = runInit(rest)
	case "reconfigure":
		err = runReconfigure(rest)
	case "join":
		err = runJoin(ctx, rest)
	case "invite":
		err = runInvite()
	case "host":
		err = runHost(rest)
	case "device":
		err = runDevice(ctx, rest)
	case "net-info": // used by install.sh
		ip, subnet, e := netinfo.LAN()
		if e != nil {
			err = e
			break
		}
		fmt.Println(ip, subnet)
	case "status":
		err = runStatus(ctx, rest)
	case "switch":
		if len(rest) != 2 {
			err = errors.New("usage: btshare switch <device> <host>")
			break
		}
		err = runSwitch(ctx, rest[0], rest[1])
	case "next", "here":
		if len(rest) > 1 {
			err = fmt.Errorf("usage: btshare %s [device]  (no device: all shared devices)", cmd)
			break
		}
		device := ""
		if len(rest) == 1 {
			device = rest[0]
		}
		err = runSwitch(ctx, device, "@"+cmd)
	case "keys":
		err = runKeys(ctx, rest)
	case "version", "--version":
		fmt.Println(version)
	case "gen-secret":
		fmt.Println(genSecret())
	case "help", "-h", "--help":
		flag.Usage()
	default:
		err = fmt.Errorf("unknown command %q (see btshare help)", cmd)
	}
	return err
}

var notifier = notify.New("btshare")

func desktopNotify(title, body string) {
	if !*notifyFlag {
		return
	}
	if strings.Contains(title, "failed") {
		notifier.Error(title, body)
	} else {
		notifier.Info(title, body)
	}
}

func loadSwitcher() (*config.Config, *switcher.Switcher, error) {
	cfg, err := config.Load(*configPath)
	if err != nil {
		return nil, nil, err
	}
	logf := debugLog()
	s := &switcher.Switcher{Self: cfg.Self, Nodes: map[string]node.Node{}, Logf: logf}
	for _, h := range cfg.Hosts {
		s.Order = append(s.Order, h.Name)
		if h.Name == cfg.Self {
			l, err := node.NewLocal(cfg.Adapter, logf)
			if err != nil {
				return nil, nil, err
			}
			s.Nodes[h.Name] = l
		} else {
			s.Nodes[h.Name] = &node.Remote{Addr: h.Addr, Secret: []byte(cfg.Secret)}
		}
	}
	return cfg, s, nil
}

func runAgent(ctx context.Context) error {
	logger := log.New(os.Stderr, "", log.LstdFlags)
	// The agent runs in the background: every failure must reach the desktop.
	fail := func(err error) error {
		notifier.Error("btshare agent stopped", err.Error())
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fail(err)
	}
	// The agent's stderr already goes to the journal (tag "btshare").
	local, err := node.NewLocal(cfg.Adapter, logger.Printf)
	if err != nil {
		return fail(err)
	}
	srv := &http.Server{
		Addr: cfg.Listen,
		Handler: (&agent.Server{
			Node: local, Secret: []byte(cfg.Secret), ConfigPath: *configPath,
			Notify: notifier, Log: logger,
		}).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		// ctx is already canceled here; shutdown gets its own deadline.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	logger.Printf("btshare agent %q listening on %s (adapter %s, %d hosts, %d devices)",
		cfg.Self, cfg.Listen, cfg.Adapter, len(cfg.Hosts), len(cfg.Devices))
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return fail(err)
	}
	return nil
}

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	name := fs.String("name", "", "this host's name (default: hostname)")
	addr := fs.String("addr", "", "address peers use to reach this host (default: LAN IP:port)")
	port := fs.String("port", config.DefaultPort, "agent port")
	secret := fs.String("secret", "", "shared secret (default: generate; use the group's secret when joining)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		h, err := os.Hostname()
		if err != nil {
			return err
		}
		*name = strings.SplitN(h, ".", 2)[0]
	}
	if *addr == "" {
		ip, _, err := netinfo.LAN()
		if err != nil {
			return fmt.Errorf("detect LAN address (pass -addr): %w", err)
		}
		*addr = ip.String() + ":" + *port
	}
	if *secret == "" {
		*secret = genSecret()
	}
	c := &config.Config{
		Self: *name, Secret: *secret, Listen: ":" + *port,
		Hosts: []config.Host{{Name: *name, Addr: *addr}},
	}
	if err := config.Create(*configPath, c); err != nil {
		return err
	}
	fmt.Printf("created %s for host %q at %s\n", *configPath, *name, *addr)
	return nil
}

// runReconfigure changes this host's name, port or secret in an existing
// config, keeping its hosts and devices. install.sh uses it when it is run
// with --name/--port/--join on a computer that already has a config.
func runReconfigure(args []string) error {
	fs := flag.NewFlagSet("reconfigure", flag.ContinueOnError)
	name := fs.String("name", "", "new name for this host")
	port := fs.String("port", "", "new agent port")
	secret := fs.String("secret", "", "new shared secret (use the group's secret when joining)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("usage: btshare reconfigure [-name N] [-port P] [-secret S]")
	}
	var changes []string
	var peers int
	_, err := config.Update(*configPath, func(c *config.Config) error {
		peers = len(c.Hosts) - 1
		if *name != "" {
			old := c.Self
			changed, err := c.RenameSelf(*name)
			if err != nil {
				return err
			}
			if changed {
				changes = append(changes, fmt.Sprintf("name %s -> %s", old, *name))
			}
		}
		if *port != "" {
			changed, err := c.SetPort(*port)
			if err != nil {
				return err
			}
			if changed {
				changes = append(changes, "port "+*port)
			}
		}
		if *secret != "" && *secret != c.Secret {
			c.Secret = *secret
			changes = append(changes, "secret")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		fmt.Println("config unchanged")
		return nil
	}
	fmt.Printf("updated %s: %s\n", *configPath, strings.Join(changes, ", "))
	if peers > 0 {
		fmt.Fprintf(os.Stderr, "warning: the %d other computer(s) in this config still have the old settings; "+
			"run `btshare join` against one of them to re-announce this computer\n", peers)
	}
	return nil
}

func withPort(addr string) string {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return net.JoinHostPort(addr, config.DefaultPort)
	}
	return addr
}

func runJoin(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("join", flag.ContinueOnError)
	secretFlag := fs.String("secret", "", "the group's secret (default: ask for it)")
	var addr string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		addr, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if addr == "" && fs.NArg() == 1 {
		addr = fs.Arg(0)
	}
	if addr == "" || fs.NArg() > 1 || (fs.NArg() == 1 && addr != fs.Arg(0)) {
		return errors.New("usage: btshare join <ip[:port]> [-secret S]")
	}
	secret := *secretFlag
	if secret == "" {
		secret = readSecret(addr)
	}
	cfg, warns, err := group.Join(ctx, *configPath, withPort(addr), secret)
	if err != nil {
		return err
	}
	for _, w := range warns {
		fmt.Fprintf(os.Stderr, "warning: could not announce this computer to %v\n", w)
	}
	fmt.Printf("joined: %d computers, %d shared devices\n", len(cfg.Hosts), len(cfg.Devices))
	for _, h := range cfg.Hosts {
		fmt.Printf("  %-12s %s\n", h.Name, h.Addr)
	}
	return nil
}

// isTerminal reports whether f is a terminal (not just any character
// device such as /dev/null).
func isTerminal(f *os.File) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCGETS,
		uintptr(unsafe.Pointer(&t))) //nolint:gosec // TCGETS only fills t, sized by the kernel's termios
	return errno == 0
}

// readSecret asks for the group secret without echoing it. Enter keeps
// this computer's current secret. Without a terminal it reads one line
// from stdin (install.sh passes nothing: it has set the secret already).
func readSecret(addr string) string {
	if !isTerminal(os.Stdin) {
		var line string
		_, _ = fmt.Fscanln(os.Stdin, &line)
		return strings.TrimSpace(line)
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	fmt.Fprintf(os.Stderr, "On %s, `btshare invite` shows the group secret.\nGroup secret (Enter keeps this computer's current one): ", host)
	stty := func(arg string) {
		c := exec.Command("stty", arg)
		c.Stdin = os.Stdin
		_ = c.Run()
	}
	stty("-echo")
	var line string
	_, _ = fmt.Fscanln(os.Stdin, &line)
	stty("echo")
	fmt.Fprintln(os.Stderr)
	return strings.TrimSpace(line)
}

func runInvite() error {
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	self, _ := cfg.Host(cfg.Self)
	fmt.Printf("On the other computer, run (from the btshare source directory):\n\n"+
		"  ./install.sh --join %s       if btshare isn't installed there yet\n"+
		"  btshare join %s              if it is\n\nand paste this secret when asked:\n\n  %s\n",
		self.Addr, self.Addr, cfg.Secret)
	return nil
}

func runHost(args []string) error {
	if len(args) != 3 || args[0] != "add" {
		return errors.New("usage: btshare host add <name> <ip[:port]>")
	}
	if err := group.AddHost(*configPath, config.Host{Name: args[1], Addr: withPort(args[2])}); err != nil {
		return err
	}
	fmt.Printf("added %s; it must use the same secret (see `btshare invite`)\n", args[1])
	return nil
}

func runDevice(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: btshare device add|remove|list")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		bz, err := bluez.New(cfg.Adapter)
		if err != nil {
			return err
		}
		defer bz.Close()
		paired, err := bz.Paired()
		if err != nil {
			return err
		}
		for _, p := range paired {
			shared := ""
			if d, err := cfg.Device(p.MAC); err == nil {
				shared = "shared as " + d.Name
			}
			conn := ""
			if p.Connected {
				conn = "connected"
			}
			fmt.Printf("%s  %-28s %-10s %s\n", p.MAC, p.Name, conn, shared)
		}
		return nil
	case "add":
		if len(args) != 3 {
			return errors.New("usage: btshare device add <name> <MAC|paired device name>")
		}
		mac, err := resolveMAC(cfg, args[2])
		if err != nil {
			return err
		}
		// Find out how this device can be shared: by switching its channel
		// if it has several, by moving its single pairing otherwise.
		var here *config.DeviceHost
		info, perr := hidpp.Probe(mac)
		switch {
		case perr == nil:
			here = &config.DeviceHost{Host: cfg.Self, MAC: mac, Channel: info.Current}
		case errors.Is(perr, hidpp.ErrNotFound):
			return fmt.Errorf("%s (%s) is not connected to this computer; connect it first, "+
				"so btshare can check whether it has switchable channels", args[1], mac)
		case !errors.Is(perr, hidpp.ErrNotSupported):
			return fmt.Errorf("check %s for switchable channels: %w", args[1], perr)
		}
		d, warns, err := group.AddDevice(ctx, *configPath, config.Device{Name: args[1], MAC: mac}, here)
		if err != nil {
			return err
		}
		for _, w := range warns {
			fmt.Fprintf(os.Stderr, "warning: not updated on %v (re-run when it's online)\n", w)
		}
		if here == nil {
			fmt.Printf("sharing %s (%s) with %d computer(s): single-host device, one pairing for all computers\n",
				d.Name, mac, len(cfg.Hosts))
			fmt.Printf("next: copy its pairing to the other computers (README, step 4b)\n")
			return nil
		}
		fmt.Printf("sharing %s with %d computer(s): it has %d channels, this computer (%s) is on channel %d\n",
			d.Name, len(cfg.Hosts), info.Channels, cfg.Self, info.Current)
		var missing []string
		for _, h := range cfg.Hosts {
			if e, ok := d.On(h.Name); ok {
				fmt.Printf("  channel %d  %-14s %s\n", e.Channel, h.Name, e.MAC)
			} else {
				fmt.Printf("  -          %-14s not set up yet\n", h.Name)
				missing = append(missing, h.Name)
			}
		}
		if len(missing) > 0 {
			fmt.Printf("next, on %s: select another channel on %s, pair it there, then run\n  btshare device add %s <MAC|paired name>\n",
				strings.Join(missing, ", "), d.Name, d.Name)
		}
		return nil
	case "remove", "rm":
		if len(args) != 2 {
			return errors.New("usage: btshare device remove <name|MAC>")
		}
		d, warns, err := group.RemoveDevice(ctx, *configPath, args[1])
		if err != nil {
			return err
		}
		for _, w := range warns {
			fmt.Fprintf(os.Stderr, "warning: not removed on %v (re-run when it's online)\n", w)
		}
		label := d.MAC
		if d.Name != "" {
			label = d.Name + " (" + d.MAC + ")"
		}
		fmt.Printf("stopped sharing %s on %d computer(s)\n", label, len(cfg.Hosts)-len(warns))
		fmt.Printf("its Bluetooth pairing is untouched; to forget it too: bluetoothctl remove %s (on each computer)\n", d.MAC)
		return nil
	}
	return fmt.Errorf("unknown device subcommand %q", args[0])
}

// resolveMAC accepts a MAC or the name of a device paired with this host.
func resolveMAC(cfg *config.Config, arg string) (string, error) {
	if mac, err := config.ValidMAC(arg); err == nil {
		return mac, nil
	}
	bz, err := bluez.New(cfg.Adapter)
	if err != nil {
		return "", err
	}
	defer bz.Close()
	paired, err := bz.Paired()
	if err != nil {
		return "", err
	}
	// Exact name matches first, then partial ones. Several devices can share
	// a name (e.g. an old pairing next to a new one): the connected one wins.
	var exact, partial []bluez.PairedDevice
	for _, p := range paired {
		if strings.EqualFold(p.Name, arg) {
			exact = append(exact, p)
		} else if strings.Contains(strings.ToLower(p.Name), strings.ToLower(arg)) {
			partial = append(partial, p)
		}
	}
	for _, hits := range [][]bluez.PairedDevice{exact, partial} {
		if len(hits) == 1 {
			return hits[0].MAC, nil
		}
		var connected []bluez.PairedDevice
		for _, h := range hits {
			if h.Connected {
				connected = append(connected, h)
			}
		}
		if len(connected) == 1 {
			return connected[0].MAC, nil
		}
		if len(hits) > 1 {
			return "", fmt.Errorf("%q matches %d paired devices; pass the MAC (see `btshare device list`)", arg, len(hits))
		}
	}
	return "", fmt.Errorf("no paired device named %q (see `btshare device list`)", arg)
}

func genSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func runStatus(ctx context.Context, args []string) error {
	cfg, s, err := loadSwitcher()
	if err != nil {
		return err
	}
	devs := cfg.Devices
	if len(args) == 1 {
		d, err := cfg.Device(args[0])
		if err != nil {
			return err
		}
		devs = []config.Device{d}
	}
	for _, d := range devs {
		if d.MultiChannel() {
			fmt.Printf("%s (switches channels)\n", d.Name)
		} else {
			fmt.Printf("%s (%s)\n", d.Name, d.MAC)
		}
		for _, r := range s.Status(ctx, d) {
			marker := " "
			if r.Host == cfg.Self {
				marker = "*"
			}
			state := describe(r)
			if d.MultiChannel() {
				if e, ok := d.On(r.Host); ok {
					state = fmt.Sprintf("channel %d  %s", e.Channel, state)
				} else {
					state = "not set up (run `btshare device add " + d.Name + " ...` there)"
				}
			}
			fmt.Printf("  %s %-12s %s\n", marker, r.Host, state)
		}
	}
	return nil
}

func describe(r switcher.HostResult) string {
	switch {
	case r.Err != nil:
		return "error: " + r.Err.Error()
	case !r.State.Known:
		return "not paired"
	case r.State.Connected:
		return "CONNECTED"
	case r.State.Blocked:
		return "released"
	default:
		return "not connected"
	}
}

// runSwitch moves one device, or every configured device when device is "".
// target is a host name, "@next" or "@here".
func runSwitch(ctx context.Context, device, target string) error {
	cfg, s, err := loadSwitcher()
	if err != nil {
		return err
	}
	devs := cfg.Devices
	if device != "" {
		d, err := cfg.Device(device)
		if err != nil {
			return err
		}
		devs = []config.Device{d}
	}
	if len(devs) == 0 {
		return errors.New("no shared devices yet (add one with `btshare device add`)")
	}
	var moved, failed []string
	for _, d := range devs {
		var rep *switcher.Report
		var err error
		switch target {
		case "@next":
			rep, err = s.Next(ctx, d)
		case "@here":
			rep, err = s.Switch(ctx, d, cfg.Self)
		default:
			rep, err = s.Switch(ctx, d, target)
		}
		if rep != nil {
			for _, r := range rep.Released {
				if r.Err != nil {
					fmt.Fprintf(os.Stderr, "warning: %s: %s: %v\n", d.Name, r.Host, r.Err)
				}
			}
		}
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", d.Name, err))
			continue
		}
		msg := fmt.Sprintf("%s → %s", d.Name, rep.Target)
		fmt.Println(msg)
		moved = append(moved, msg)
	}
	if len(moved) > 0 {
		desktopNotify("btshare", strings.Join(moved, "\n"))
	}
	if len(failed) > 0 {
		return errors.New(strings.Join(failed, "; "))
	}
	return nil
}

func runKeys(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: btshare keys export|import")
	}
	fs := flag.NewFlagSet("keys "+args[0], flag.ContinueOnError)
	root := fs.String("root", keys.DefaultRoot, "BlueZ storage directory")
	adapter := fs.String("adapter", "hci0", "local adapter")
	switch args[0] {
	case "export":
		out := fs.String("o", "", "output file (default stdout)")
		if len(args) < 2 {
			return errors.New("usage: btshare keys export <device|MAC> [-o file]")
		}
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		mac := strings.ToUpper(args[1])
		if len(mac) != 17 || strings.Count(mac, ":") != 5 {
			cfg, err := config.Load(*configPath)
			if err != nil {
				return fmt.Errorf("%q is not a MAC and config is unavailable (pass -config or the MAC): %w", args[1], err)
			}
			d, err := cfg.Device(args[1])
			if err != nil {
				return err
			}
			mac = d.MAC
		}
		addr, err := adapterAddress(*adapter)
		if err != nil {
			return err
		}
		b, err := keys.Export(*root, addr, mac)
		if err != nil {
			return err
		}
		w := os.Stdout
		if *out != "" {
			f, err := os.OpenFile(*out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return err
			}
			defer f.Close()
			if err := chownToSudoUser(f); err != nil {
				return err
			}
			w = f
		}
		if err := b.Write(w); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "exported bond for %s (adapter %s). This file contains secret keys: move it securely and delete it after import.\n", mac, addr)
		return nil

	case "import":
		setAddr := fs.Bool("set-address", false, "change this adapter's address to the bundle's (btmgmt public-addr)")
		noRestart := fs.Bool("no-restart", false, "don't restart bluetooth.service")
		if len(args) < 2 {
			return errors.New("usage: btshare keys import <file> [-set-address] [-no-restart]")
		}
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		f, err := os.Open(args[1])
		if err != nil {
			return err
		}
		defer f.Close()
		b, err := keys.Read(f)
		if err != nil {
			return err
		}
		local, err := adapterAddress(*adapter)
		if err != nil {
			return err
		}
		if !strings.EqualFold(local, b.AdapterAddress) {
			if !*setAddr {
				return fmt.Errorf("this adapter is %s but the device is bonded to %s.\n"+
					"The device only reconnects to the address it was paired with, so this host must use it too.\n"+
					"Re-run with -set-address (needs a controller that supports changing its public address)", local, b.AdapterAddress)
			}
			if err := setPublicAddress(*adapter, b.AdapterAddress); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "adapter %s address set to %s (not persistent across reboots; see README)\n", *adapter, b.AdapterAddress)
		}
		paths, err := keys.Install(*root, b)
		if err != nil {
			return err
		}
		for _, p := range paths {
			fmt.Fprintln(os.Stderr, "wrote", p)
		}
		if !*noRestart {
			if out, err := exec.CommandContext(ctx, "systemctl", "restart", "bluetooth").CombinedOutput(); err != nil {
				return fmt.Errorf("restart bluetooth: %w: %s", err, out)
			}
			fmt.Fprintln(os.Stderr, "restarted bluetooth.service")
		}
		return nil
	}
	return fmt.Errorf("unknown keys subcommand %q", args[0])
}

// chownToSudoUser gives a file created under sudo back to the invoking
// user, so they can copy it to the other computer.
func chownToSudoUser(f *os.File) error {
	if os.Geteuid() != 0 {
		return nil
	}
	uid, err1 := strconv.Atoi(os.Getenv("SUDO_UID"))
	gid, err2 := strconv.Atoi(os.Getenv("SUDO_GID"))
	if err1 != nil || err2 != nil {
		return nil
	}
	return f.Chown(uid, gid)
}

func adapterAddress(adapter string) (string, error) {
	c, err := bluez.New(adapter)
	if err != nil {
		return "", err
	}
	defer c.Close()
	return c.AdapterAddress()
}

// btmgmtTimeout bounds each btmgmt call; a healthy one answers in well under a second.
const btmgmtTimeout = 10 * time.Second

// runBtmgmt runs one btmgmt command. btmgmt (BlueZ 5.8x) hangs forever when
// stdin is /dev/null, which is what exec gives a nil Stdin (it can't epoll
// that file), so hand it an empty pipe instead, and time it out regardless.
func runBtmgmt(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), btmgmtTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "btmgmt", args...)
	cmd.Stdin = strings.NewReader("") // not an *os.File, so exec uses a pipe
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("no answer after %s (is bluetooth.service running?)", btmgmtTimeout)
	}
	return out, err
}

func setPublicAddress(adapter, addr string) error {
	for _, a := range [][]string{
		{"--index", adapter, "power", "off"},
		{"--index", adapter, "public-addr", addr},
		{"--index", adapter, "power", "on"},
	} {
		out, err := runBtmgmt(a...)
		if err != nil {
			return fmt.Errorf("btmgmt %s: %w: %s", strings.Join(a, " "), err, out)
		}
	}
	got, err := adapterAddress(adapter)
	if err != nil {
		return err
	}
	if !strings.EqualFold(got, addr) {
		return fmt.Errorf("controller did not accept the new address (still %s); it likely doesn't support public-addr", got)
	}
	return nil
}

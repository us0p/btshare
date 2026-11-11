# btshare

Share one Bluetooth device (mouse, keyboard, headset…) between several Linux
computers on the same LAN. Run one command (or press a hotkey) and the device
disconnects from the computer holding it and connects to the one you chose.
It is never deliberately connected to two computers at once.

```
$ btshare status
mouse (AA:BB:CC:DD:EE:7F)
  * desktop      CONNECTED
    laptop       released

$ btshare switch mouse laptop     # move it to a specific computer
$ btshare next mouse              # cycle to the next computer
$ btshare here                    # pull every shared device to this computer
```

## Install

Requirements: Linux with BlueZ and systemd, Go 1.22+ to build.

**First computer** (creates the group):

```sh
./install.sh
```

**Every other computer** (joins the group):

```sh
btshare invite                     # on a computer that's already set up: shows the command + secret
./install.sh --join 192.168.1.10   # on the new computer; paste the secret when asked
```

The installer:

1. builds btshare and, after asking, installs it root-owned to `/usr/local/bin`,
   which is on sudo's path, so `sudo btshare keys ...` works. If you decline, it installs
   to `~/.local/bin` and you run `sudo ~/.local/bin/btshare keys ...` instead;
2. creates `~/.config/btshare/config.toml` listing only this computer
   (hostname, LAN address, and a new secret, or the group's when joining);
3. detects **ufw** or **firewalld**, shows the exact rule that allows the agent port
   (TCP 7531) from your LAN subnet, and runs it with `sudo` **only if you approve**;
4. installs and starts the `btshare-agent` systemd user service;
5. with `--join`, registers with the group. The computer you join appends
   this one to its config, this one appends every existing member, and every
   other member is told about the new one;
6. on GNOME, offers the shortcut **Super+Alt+B** for `btshare -notify next`.

**Updating:** re-run `./install.sh`. If the source is a git checkout, it shows
new commits on the remote and offers to pull them (skipped when you have local
changes), then rebuilds, reinstalls, restarts the agent and prints
`Updated btshare <old> → <new>`. Your config, firewall rule and shortcut are
kept, and you aren't asked again about steps you already answered.
`btshare version` shows what's installed.
Passing `--name`, `--port` or `--join` on a computer that already has a config
updates it in place (`btshare reconfigure`), keeping its hosts and devices;
with `--join` you're asked for the group secret (Enter keeps the current one).
To start over instead, `./install.sh --fresh [--join ADDR]` moves the config to
`config.toml.bak-<date>` (after asking) and sets the computer up as on a first
install. Bluetooth pairings, the firewall rule and the shortcut are kept.
Options: `--name`, `--port`, `--no-firewall`, `--no-hotkey`, `--yes`;
`./install.sh --uninstall [--purge]` removes everything, including the firewall rule
(after asking).

### Share a device

```sh
btshare device list                    # devices paired with this computer
btshare device add mouse "MX Vertical" # by name or MAC; added on every computer
btshare device remove mouse            # stop sharing it, on every computer
```

Re-pairing some mice (e.g. Logitech) gives them a new address. Then `device add`
the new one and `device remove` the old one. Removing never touches Bluetooth
pairings (`bluetoothctl remove <MAC>` does).

New hosts and devices are appended to each computer's config file. You can also
add a host by hand with `btshare host add <name> <ip>`. Config files are rewritten
by btshare, so comments you add will be lost.

### Pairing (required for single-host devices)

A Bluetooth peripheral only connects to hosts it has a **bond** with, meaning
that host's adapter address plus shared keys. Most single-host mice keep
only one bond, so pairing with computer B erases computer A's. Every computer
must therefore look like the same host to the device:

```sh
# On the computer where the device is paired:
sudo btshare keys export mouse -o mouse.bundle.json   # file is owned by you, not root
# Copy it securely (it contains secret keys), then on each other computer:
sudo btshare keys import mouse.bundle.json -set-address
shred -u mouse.bundle.json
```

- `-set-address` gives this adapter the other computer's address (`btmgmt public-addr`).
  Not every controller supports this; btshare checks and tells you. The change
  is lost on reboot, so use `contrib/btshare-bdaddr.service` to apply it at boot.
- Two adapters with the same address can both answer a device that's reconnecting.
  Released computers block the device, so they reject it. If a switch is
  flaky, run it again or move the mouse so it wakes up.
- Multi-host devices (e.g. Logitech Easy-Switch) can instead be paired normally on
  each computer, but btshare can only connect on the channel selected on the device.

## Notifications

The agent runs in the background, so everything that goes wrong is shown as a
desktop notification. It sends them through the freedesktop notification
service (GNOME, KDE, dunst, mako…), falling back to `notify-send`. This covers:

- the agent failing to start (bad config, port in use, BlueZ unavailable);
- a request with a wrong secret or clock skew, showing the sender's IP;
- failing to connect or release a device when another computer asked;
- a request for a device that isn't in this computer's config;
- a computer failing to join. A successful join shows a short info message.

Identical messages are shown at most once a minute. systemd stops restarting
an agent that keeps crashing, so you get a few notifications, not an endless stream.
CLI commands report through notifications too when run with `-notify`, as the hotkey does.
Logs: `journalctl --user -u btshare-agent -f`.

### Troubleshooting a switch

Every switch logs a detailed trace to the journal on each computer involved:
who held the device, each release and connect with its duration, BlueZ's exact
error per `Connect()` attempt, and every device property change BlueZ signals
meanwhile (`Connected`, `ServicesResolved`, ...). Read it with
`journalctl -t btshare --since "-10 min"`, or watch it live with `btshare -v next`.
A connect that stays "pending" with no property changes means the device was
never heard from: it isn't advertising to this computer.

## How it works

Each computer runs `btshare agent`, an HTTP service controlling local BlueZ
over D-Bus. A switch first asks the **target** to *acquire* the device (`Blocked=false`,
`Trusted=true`, `Connect()` with retries), and after a 0.5 s head start asks every
**other** computer to *release* it (`Disconnect()` + `Blocked=true`, keeping the
bond). The target is already listening when the device drops its old
connection, which matters for BLE devices that advertise only briefly after a
disconnect. If the target can't connect, the device is handed back the same way.
Offline computers are skipped. Requests are signed with HMAC-SHA256 using the
shared secret plus a timestamp, so stale requests are rejected after 30 s; keep
clocks in sync with NTP. Agents only touch devices listed in their config.

| Path | Purpose |
|---|---|
| `install.sh` | Installer / uninstaller |
| `cmd/btshare` | CLI and agent entry point |
| `internal/bluez` | BlueZ D-Bus calls: state / release / acquire / list |
| `internal/node` | Wire protocol; local BlueZ or remote agent |
| `internal/agent` | Authenticated HTTP server, membership updates, error notifications |
| `internal/group` | join / device add, keeping configs in sync |
| `internal/config` | Config file, locked atomic updates |
| `internal/notify` | Desktop notifications |
| `internal/switcher` | Switch / next logic |
| `internal/keys` | Export/import of BlueZ bonds |

# btshare

Share one Bluetooth device (mouse, keyboard, headset…) between several Linux
computers on the same LAN. Run one command (or press a hotkey) and the device
leaves the computer holding it and connects to the one you chose.
It is never deliberately connected to two computers at once.

btshare works with two kinds of devices and detects which one you have:

- **Multi-channel devices** remember several computers, one per channel, and
  have a button to pick one (Logitech Easy-Switch: the 1-2-3 button). Each
  computer pairs the device normally on its own channel; btshare switches by
  telling the device to change channel, as if you pressed the button.
- **Single-host devices** remember one computer. btshare copies that pairing
  to the other computers and moves the connection between them.

```
$ btshare status
mx (switches channels)
  * desktop      channel 1  CONNECTED
    laptop       channel 2  not connected

$ btshare switch mouse laptop     # move it to a specific computer
$ btshare next mouse              # cycle to the next computer
$ btshare here                    # pull every shared device to this computer
```

## Setup, step by step

Requirements: Linux with BlueZ and systemd, Go 1.22+ to build. The examples
use two computers, **desktop** (192.168.1.10) and **laptop**; repeat the
laptop steps for each further computer.

### 1. Install on the first computer

```sh
git clone https://github.com/us0p/btshare && cd btshare
./install.sh
```

The installer asks before each step that needs sudo:

1. builds btshare and installs it to `/usr/local/bin` (root-owned, so `sudo btshare keys ...`
   works). If you decline, it goes to `~/.local/bin` and you run `sudo ~/.local/bin/btshare keys ...`;
2. creates `~/.config/btshare/config.toml`: this computer's name (hostname by
   default, `--name` to choose), LAN address, and a new group secret;
3. opens TCP 7531 from your LAN subnet in **ufw** or **firewalld**, showing the exact rule first;
4. installs a udev rule (`/etc/udev/rules.d/70-btshare-hidraw.rules`) that lets
   your user talk to Logitech Bluetooth devices, which multi-channel switching needs;
5. installs and starts the `btshare-agent` systemd user service;
6. on GNOME, offers the shortcut **Super+Alt+B** for `btshare -notify next`.

### 2. Add the other computers

On the first computer, show the join command and the group secret:

```sh
btshare invite
```

On the laptop, run the command it prints and paste the secret when asked:

```sh
git clone https://github.com/us0p/btshare && cd btshare
./install.sh --join 192.168.1.10
```

Both computers now list each other (`btshare status` shows both). If two
computers have the same hostname, give one another name with `--name`.

If btshare is already installed on the laptop (for example it was set up on
its own, with its own secret), join without reinstalling:

```sh
btshare join 192.168.1.10      # asks for the secret; Enter keeps the laptop's current one
```

The secret is saved only if the other computer accepts it. "bad signature"
means it doesn't match: copy it again from `btshare invite`. Run `--fresh` or
`init` on one computer only; every other computer joins its group.

### 3. Pair the device with the first computer

Pair it as usual (GNOME Settings → Bluetooth, or `bluetoothctl`). For a
multi-channel device, select channel 1 with its button first.

### 4. Share it

On the first computer, with the device connected:

```sh
btshare device list                    # paired devices, to find its name or MAC
btshare device add mouse "MX Vertical" # give it a short name; by paired name or MAC
```

`device add` checks the device and tells you which kind it is. Follow 4a or
4b depending on what it prints.

#### 4a. Multi-channel device ("it has N channels")

```
sharing mouse with 2 computer(s): it has 3 channels, this computer (desktop) is on channel 1
  channel 1  desktop        AA:BB:CC:DD:EE:83
  -          laptop         not set up yet
```

Then, for each other computer:

1. Press the device's channel button to select a **free** channel (e.g. 2).
2. Put it in pairing mode on that channel (Logitech: hold the button ~3 s
   until the light blinks fast).
3. Pair it with the laptop as usual.
4. On the laptop, with the device connected:

   ```sh
   btshare device add mouse "MX Vertical"
   ```

   It prints the laptop's channel. Use the same name as on the first computer.
   The device may have a different address on each channel; that's expected,
   btshare stores one per computer.
5. Press the button to go back to the first computer's channel, or just run
   `btshare here mouse` on it.

Nothing else is needed: no keys are copied and adapters keep their own addresses.

#### 4b. Single-host device ("one pairing for all computers")

The device keeps a single **bond** (a computer's adapter address plus shared
keys), so pairing it with the laptop would erase the desktop's. Instead every
computer must look like the same computer to it:

```sh
# On the first computer:
sudo btshare keys export mouse -o mouse.bundle.json   # file is owned by you, not root
# Copy it securely (it contains secret keys), then on each other computer:
sudo btshare keys import mouse.bundle.json -set-address
shred -u mouse.bundle.json
```

- `-set-address` gives this adapter the first computer's address (`btmgmt public-addr`).
  Not every controller supports this; btshare checks and tells you. The change
  is lost on reboot, so use `contrib/btshare-bdaddr.service` to apply it at boot.
- Two adapters with the same address can both answer a device that's reconnecting.
  Released computers block the device, so they reject it. If a switch is
  flaky, run it again or move the mouse so it wakes up.

### 5. Switch

```sh
btshare status                # who has what
btshare switch mouse laptop   # move it to a specific computer
btshare next                  # move every shared device to the next computer (what the hotkey runs)
btshare next mouse            # move just this one to the computer after the one that has it
btshare here                  # pull every shared device to this computer
```

Any computer can run these, including one that doesn't have the device.

With several devices, `btshare next` moves them together: everything goes
to the computer after this one if anything is connected here, otherwise to
the computer after the one that has them. A device that is already there
is left alone, and so is one that isn't connected anywhere (usually because
it's turned off), so switching doesn't stall waiting for it.

### Managing devices

```sh
btshare device remove mouse    # stop sharing it, on every computer
```

Removing never touches Bluetooth pairings (`bluetoothctl remove <MAC>` does),
but it unblocks the device on every computer, so one that btshare had
released isn't left unusable there.
If a single-host device was re-paired and got a new address (some Logitech
mice do), run `btshare device add` with the same name again. For a
multi-channel device, re-run `device add` on the computer whose channel was re-paired.

New hosts and devices are appended to each computer's config file. You can also
add a host by hand with `btshare host add <name> <ip>`. Config files are rewritten
by btshare, so comments you add will be lost.

### Updating and reinstalling

Re-run `./install.sh`. If the source is a git checkout, it shows
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
`./install.sh --uninstall [--purge]` removes everything, including the firewall
rule and the udev rule (after asking).

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

Single-host devices:

- *"is free to connect only to X now"*: the switch failed and nothing could
  take the device back (it was asleep, or the rollback failed too). It is
  unblocked on X and blocked everywhere else, so it isn't lost: click or move
  it to wake it and it connects to X. To have it elsewhere, run `btshare here`
  on that computer. Don't forget and re-pair it: that gives it new keys (and,
  for some mice, a new address), so every computer needs the keys again.
- *"a switch is already running on this computer"*: you pressed the hotkey
  (or button) again while a switch was still connecting. Switches on one
  computer run one at a time, so two can't fight over a device and leave it
  blocked everywhere. Wait for the first to finish or fail (under a minute).
- *"not set up on X"* from `btshare next`: X doesn't have the pairing. Copy it
  with `btshare keys export` / `keys import` (step 4b).

Triggering a switch from a device's own button (for example a Solaar rule
running `btshare -notify next`) works best with `next` and no device name:
all devices move together, so the mouse with the button comes along and you
can press it again on the other computer. Keep the keyboard shortcut set up
on every computer as a fallback.

Multi-channel devices:

- *"is not connected to this computer"* on `device add`: connect it first;
  btshare reads the channel from the device itself.
- *"no permission to use /dev/hidrawN"*: the udev rule is missing. Re-run
  `./install.sh`, or install `contrib/70-btshare-hidraw.rules` as shown there.
- *"press its channel button"*: the device went to a channel whose computer
  didn't connect (asleep, Bluetooth off, or not paired on that channel).
  No computer is connected to it, so only the button can bring it back.
- *"already uses channel N"*: two computers can't share a channel; pair the
  device with this one on another channel.

## How it works

Each computer runs `btshare agent`, an HTTP service controlling local BlueZ
over D-Bus and, for multi-channel devices, the device's `/dev/hidraw` node.

**Multi-channel devices.** `device add` sends the device a HID++ 2.0
request (feature CHANGE_HOST, 0x1814) to read its channel count and current
channel, and records `this computer = channel N, address M`. A switch asks
the target to start connecting, then asks the computer holding the device to
send it *setCurrentHost(target's channel)*. The device drops that computer and
connects to the target by itself. Nothing is blocked, so the device's own
button keeps working.

**Single-host devices.** A switch first asks the **target** to *acquire* the device (`Blocked=false`,
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
| `internal/hidpp` | Multi-channel detection and switching (Logitech HID++ 2.0) |

## Contributing

CI (`.github/workflows/ci.yml`) runs on every push and pull request, and
weekly: golangci-lint with the formatters (`.golangci.yml`), unit tests with
the race detector on the minimum Go version from `go.mod` and on the latest
release, `go mod tidy`/`verify`, a build, shellcheck on `install.sh`, and
govulncheck. Run the same checks locally before pushing:

```sh
make fmt     # format Go code (gofmt + goimports)
make check   # everything CI runs (shellcheck must be installed)
```

`.editorconfig` sets indentation and line endings for most editors.

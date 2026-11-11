#!/usr/bin/env bash
# btshare installer. Run as your normal user; sudo is requested (with your
# confirmation) to install the binary system-wide and to open the firewall.
# Re-run it at any time to update: it pulls new commits (if this is a git
# checkout), rebuilds, reinstalls, and restarts the agent, keeping your config.
#
#   ./install.sh                     first computer: creates a new group
#   ./install.sh --join 10.0.0.5     other computers: join the group of 10.0.0.5
#   ./install.sh                     later: update this computer
#   ./install.sh --fresh [--join …]  start over: discard this computer's config
#   ./install.sh --uninstall [--purge]
set -euo pipefail

ORIG_ARGS=("$@")
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Root-owned and in sudo's secure_path, so `sudo btshare keys ...` works.
SYS_BIN="${BTSHARE_SYS_BIN:-/usr/local/bin/btshare}"
# Fallback when system-wide install is declined (also where old versions lived).
USER_BIN="${PREFIX:-$HOME/.local}/bin/btshare"
CONFIG="${BTSHARE_CONFIG:-${XDG_CONFIG_HOME:-$HOME/.config}/btshare/config.toml}"
STATE="${XDG_STATE_HOME:-$HOME/.local/state}/btshare/install.state"
UNIT_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
UNIT="btshare-agent.service"
UDEV_RULE="/etc/udev/rules.d/70-btshare-hidraw.rules"
HOTKEY_PATH="/org/gnome/settings-daemon/plugins/media-keys/custom-keybindings/btshare/"
MK="org.gnome.settings-daemon.plugins.media-keys"

JOIN="" NAME="" PORT="7531" PORT_SET=0 FIREWALL=1 HOTKEY=1 PULL=1 UNINSTALL=0 PURGE=0 FRESH=0 ASSUME_YES=0

bold() { printf '\033[1m%s\033[0m\n' "$*"; }
step() { printf '\n\033[1;34m==>\033[0m \033[1m%s\033[0m\n' "$*"; }
warn() { printf '\033[1;33mwarning:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }
interactive() { [[ -t 0 && -t 1 ]]; }

# confirm "question" -> 0 if yes. Defaults to no; non-interactive is no
# unless --yes was given.
confirm() {
	((ASSUME_YES)) && return 0
	interactive || return 1
	local reply
	read -rp "$1 [y/N] " reply
	[[ $reply =~ ^[Yy]([Ee][Ss])?$ ]]
}

usage() {
	sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'
	cat <<EOF

Options:
  --join ADDR      join the group of the computer at ADDR (ip or ip:port)
  --name NAME      this computer's name in the group (default: hostname)
                   --name, --port and --join also update an existing config
  --port PORT      agent port (default 7531)
  --no-pull        don't check the git remote for updates
  --no-firewall    don't offer to open the firewall port
  --no-hotkey      don't offer to create the GNOME keyboard shortcut
  --yes            answer yes to every confirmation (sudo may still ask for your password)
  --fresh          start over: back up and remove this computer's config (name,
                   secret, computers, devices) and saved answers, then set up
                   again as on a first install
  --uninstall      remove btshare (keeps config unless --purge)
EOF
}

while (($#)); do
	case $1 in
	--join) JOIN=${2:?--join needs an address}; shift ;;
	--name) NAME=${2:?--name needs a value}; shift ;;
	--port) PORT=${2:?--port needs a value}; PORT_SET=1; shift ;;
	--no-pull) PULL=0 ;;
	--no-firewall) FIREWALL=0 ;;
	--no-hotkey) HOTKEY=0 ;;
	--yes | -y) ASSUME_YES=1 ;;
	--uninstall) UNINSTALL=1 ;;
	--purge) PURGE=1 ;;
	--fresh) FRESH=1 ;;
	-h | --help) usage; exit 0 ;;
	*) die "unknown option $1 (see --help)" ;;
	esac
	shift
done

((FRESH && UNINSTALL)) && die "--fresh and --uninstall don't go together (use --uninstall --purge to remove everything)"
[[ $EUID -ne 0 ]] || die "run as your normal user, not root; sudo is requested only when needed"
[[ $PORT =~ ^[0-9]+$ ]] && ((PORT > 0 && PORT < 65536)) || die "invalid port $PORT"

# ------------------------------------------------------------------- state
# Remembers what earlier runs did, so updates don't ask the same questions.

state_get() { sed -n "s/^$1=//p" "$STATE" 2>/dev/null | tail -1; }
state_set() {
	mkdir -p "$(dirname "$STATE")"
	{ grep -v "^$1=" "$STATE" 2>/dev/null || true; echo "$1=$2"; } >"$STATE.tmp"
	mv "$STATE.tmp" "$STATE"
}

# Path of the currently installed binary, if any.
installed_bin() {
	if [[ -x $SYS_BIN ]]; then echo "$SYS_BIN"; elif [[ -x $USER_BIN ]]; then echo "$USER_BIN"; fi
}

# ---------------------------------------------------------------- firewall

firewall_kind() {
	if command -v ufw >/dev/null && grep -qs "^ENABLED=yes" "${BTSHARE_UFW_CONF:-/etc/ufw/ufw.conf}"; then
		echo ufw
	elif command -v firewall-cmd >/dev/null && systemctl is-active --quiet firewalld; then
		echo firewalld
	fi
}

# firewall_cmds open|close SUBNET PORT -> prints one command per line
firewall_cmds() {
	local action=$1 subnet=$2 port=$3
	case $(firewall_kind) in
	ufw)
		if [[ $action == open ]]; then
			echo "sudo ufw allow from $subnet to any port $port proto tcp comment btshare"
		else
			echo "sudo ufw delete allow from $subnet to any port $port proto tcp"
		fi ;;
	firewalld)
		local verb=--add-rich-rule
		[[ $action == open ]] || verb=--remove-rich-rule
		echo "sudo firewall-cmd --permanent $verb='rule family=ipv4 source address=$subnet port port=$port protocol=tcp accept'"
		echo "sudo firewall-cmd --reload" ;;
	esac
}

configure_firewall() {
	local action=$1 port=$2 ip subnet cmds rule
	read -r ip subnet < <("$BIN" net-info) || { warn "could not detect LAN subnet; skipping firewall"; return; }
	if [[ -z $(firewall_kind) ]]; then
		echo "No active ufw or firewalld found, so there's nothing to $action."
		echo "If you filter traffic with your own nftables/iptables rules, allow TCP $port from $subnet."
		return
	fi
	rule="$(firewall_kind) $subnet $port"
	if [[ $action == open && $(state_get firewall) == "$rule" ]]; then
		echo "Already allowed: TCP $port from $subnet ($(firewall_kind))."
		return
	fi
	if [[ $action == close ]]; then
		# Close the rule we actually opened, even if the network changed since.
		local saved
		saved=$(state_get firewall)
		[[ -n $saved ]] || { echo "No firewall rule was opened by btshare."; return; }
		read -r _ subnet port <<<"$saved"
	fi
	cmds=$(firewall_cmds "$action" "$subnet" "$port")
	if [[ $action == open ]]; then
		echo "The agent on this computer ($ip) listens on TCP $port. The other computers"
		echo "on $subnet must be able to reach it. Detected firewall: $(firewall_kind)."
	else
		echo "Remove the btshare firewall rule for TCP $port from $subnet?"
	fi
	echo "This will run:"
	sed 's/^/    /' <<<"$cmds"
	if confirm "Run with sudo now?"; then
		while read -r c; do eval "$c"; done <<<"$cmds"
		if [[ $action == open ]]; then state_set firewall "$rule"; else state_set firewall ""; fi
	else
		warn "skipped; run the command(s) above yourself, or other computers can't reach this one"
	fi
}

# -------------------------------------------------------------------- udev

UDEV_RELOAD="sudo udevadm control --reload && sudo udevadm trigger --subsystem-match=hidraw --action=change"

# Multi-channel devices (Logitech Easy-Switch) are detected and switched
# through /dev/hidraw, which is root-only by default.
configure_udev() {
	local src="$SRC/contrib/70-btshare-hidraw.rules"
	if [[ -f $UDEV_RULE ]] && cmp -s "$src" "$UDEV_RULE"; then
		echo "Already installed: $UDEV_RULE"
		return
	fi
	if [[ $(state_get udev) == declined ]]; then
		echo "Skipped (declined earlier). Multi-channel devices can't be switched without it; to install:"
		echo "    sudo install -m 0644 $src $UDEV_RULE && $UDEV_RELOAD"
		return
	fi
	echo "Devices with several channels (e.g. Logitech Easy-Switch mice and keyboards) are"
	echo "switched by sending them a command through /dev/hidraw, which only root can use."
	echo "This udev rule gives the logged-in user access to Logitech Bluetooth devices only."
	echo "This will run:"
	echo "    sudo install -m 0644 $src $UDEV_RULE"
	echo "    $UDEV_RELOAD"
	if confirm "Run with sudo now?"; then
		sudo install -m 0644 "$src" "$UDEV_RULE"
		eval "$UDEV_RELOAD"
		state_set udev installed
	else
		state_set udev declined
		warn "skipped; single-host devices still work, multi-channel ones can't be detected or switched"
	fi
}

# ------------------------------------------------------------------ hotkey

gnome_hotkeys_available() {
	command -v gsettings >/dev/null && gsettings list-keys "$MK" 2>/dev/null | grep -qx custom-keybindings
}

hotkey_list_edit() { # add|remove
	local cur new
	cur=$(gsettings get "$MK" custom-keybindings)
	if [[ $1 == add ]]; then
		[[ $cur == *"'$HOTKEY_PATH'"* ]] && return
		if [[ $cur == "@as []" || $cur == "[]" ]]; then new="['$HOTKEY_PATH']"; else new="${cur%]}, '$HOTKEY_PATH']"; fi
	else
		new=$(sed -e "s|'$HOTKEY_PATH'||" -e 's/\[, /[/' -e 's/, ,/,/' -e 's/, \]/]/' <<<"$cur")
		[[ $new == "[]" ]] && new="@as []"
	fi
	gsettings set "$MK" custom-keybindings "$new"
}

configure_hotkey() {
	local want="$BIN -notify next"
	gnome_hotkeys_available || {
		echo "GNOME not detected. Bind this command to a key in your desktop's settings:"
		echo "    $want"
		return
	}
	local schema="$MK.custom-keybinding:$HOTKEY_PATH" cur
	cur=$(gsettings get "$schema" command 2>/dev/null || true)
	if [[ $cur == *btshare* ]]; then
		if [[ $cur != "'$want'" ]]; then
			gsettings set "$schema" command "$want"
			echo "Updated shortcut $(gsettings get "$schema" binding) to run: $want"
		else
			echo "Keyboard shortcut already set: $(gsettings get "$schema" binding)"
		fi
		return
	fi
	if [[ $(state_get hotkey) == declined ]]; then
		echo "Shortcut previously declined. Command to bind: $want"
		return
	fi
	if confirm "Create the keyboard shortcut Super+Alt+B to move shared devices to the next computer?"; then
		gsettings set "$schema" name 'btshare: move devices to next computer'
		gsettings set "$schema" command "$want"
		gsettings set "$schema" binding '<Super><Alt>b'
		hotkey_list_edit add
		state_set hotkey created
		echo "Shortcut created (change it in Settings → Keyboard → Custom Shortcuts)."
	else
		state_set hotkey declined
		echo "Skipped. Command to bind later: $want"
	fi
}

# ------------------------------------------------------------ self-update

# If this is a git checkout tracking a remote, offer to pull new commits and
# restart the installer from the updated source.
self_update() {
	((PULL)) && [[ -z ${BTSHARE_INSTALLER_UPDATED:-} ]] || return 0
	git -C "$SRC" rev-parse --is-inside-work-tree >/dev/null 2>&1 || return 0
	local upstream behind
	upstream=$(git -C "$SRC" rev-parse --abbrev-ref --symbolic-full-name '@{u}' 2>/dev/null) || return 0
	step "Checking $upstream for updates"
	if ! git -C "$SRC" fetch --quiet 2>/dev/null; then
		warn "could not reach the git remote; installing the current source"
		return 0
	fi
	behind=$(git -C "$SRC" rev-list --count 'HEAD..@{u}')
	if ((behind == 0)); then
		echo "source is up to date"
		return 0
	fi
	if [[ -n $(git -C "$SRC" status --porcelain --untracked-files=no) ]]; then
		warn "$behind new commit(s) on $upstream, but you have local changes; not pulling"
		return 0
	fi
	git -C "$SRC" log --oneline 'HEAD..@{u}' | sed 's/^/    /'
	if confirm "Pull these $behind commit(s) before installing?"; then
		git -C "$SRC" pull --ff-only --quiet || die "git pull failed; resolve it and re-run"
		# git replaced install.sh on disk; run the new version.
		BTSHARE_INSTALLER_UPDATED=1 exec "$SRC/install.sh" "${ORIG_ARGS[@]}"
	fi
}

# ------------------------------------------------------------ install bin

install_binary() {
	local built=$1 old_bin
	old_bin=$(installed_bin)
	CHANGED=1
	[[ -n $old_bin ]] && cmp -s "$built" "$old_bin" && CHANGED=0

	if [[ $(state_get bin) != user ]]; then
		if [[ -x $SYS_BIN ]] && cmp -s "$built" "$SYS_BIN"; then
			echo "$SYS_BIN is already up to date"
			BIN=$SYS_BIN
		else
			if [[ $(state_get bin) != system ]]; then
				echo "btshare is installed to $SYS_BIN (owned by root) so that it's also"
				echo "available to sudo, which 'sudo btshare keys ...' needs. This will run:"
				echo "    sudo install -o root -g root -m 0755 $built $SYS_BIN"
			fi
			# Approval is remembered: updates then only need your sudo password.
			if [[ $(state_get bin) == system ]] || confirm "Install system-wide with sudo?"; then
				sudo install -o root -g root -m 0755 "$built" "$SYS_BIN"
				state_set bin system
				BIN=$SYS_BIN
				echo "installed $SYS_BIN"
			fi
		fi
	fi

	if [[ ${BIN:-} != "$SYS_BIN" ]]; then
		install -Dm755 "$built" "$USER_BIN"
		BIN=$USER_BIN
		state_set bin user
		echo "installed $USER_BIN"
		warn "sudo can't find $USER_BIN; use 'sudo $USER_BIN keys ...' (re-run with BTSHARE_RESET_BIN=1 ./install.sh to install system-wide)"
		case ":$PATH:" in *":$(dirname "$USER_BIN"):"*) ;; *) warn "$(dirname "$USER_BIN") is not in your PATH" ;; esac
	elif [[ -e $USER_BIN ]]; then
		# A copy from an older install would shadow the system one in PATH.
		rm -f "$USER_BIN"
		echo "removed old copy $USER_BIN"
	fi
	hash -r

	if [[ -n $old_bin ]]; then
		local new_ver
		new_ver=$("$BIN" version)
		if [[ $OLD_VERSION != "$new_ver" ]]; then
			bold "Updated btshare $OLD_VERSION → $new_ver"
		elif [[ $CHANGED == 1 ]]; then
			bold "Reinstalled btshare $new_ver (rebuilt from modified source)"
		fi
	fi
}

# --------------------------------------------------------------- uninstall

if ((UNINSTALL)); then
	BIN=$(installed_bin)
	step "Stopping the agent"
	systemctl --user disable --now "$UNIT" 2>/dev/null || true
	rm -f "$UNIT_DIR/$UNIT"
	systemctl --user daemon-reload
	if gnome_hotkeys_available && [[ $(gsettings get "$MK.custom-keybinding:$HOTKEY_PATH" command 2>/dev/null) == *btshare* ]]; then
		step "Removing keyboard shortcut"
		gsettings reset-recursively "$MK.custom-keybinding:$HOTKEY_PATH"
		hotkey_list_edit remove
	fi
	if [[ -n $BIN ]]; then
		step "Firewall"
		configure_firewall close "$PORT"
	fi
	if [[ -f $UDEV_RULE ]]; then
		step "Removing udev rule"
		echo "This will run: sudo rm -f $UDEV_RULE"
		if confirm "Remove it?"; then sudo rm -f "$UDEV_RULE"; else warn "kept $UDEV_RULE"; fi
	fi
	step "Removing binaries"
	rm -f "$USER_BIN"
	if [[ -e $SYS_BIN ]]; then
		echo "This will run: sudo rm -f $SYS_BIN"
		if confirm "Remove it?"; then sudo rm -f "$SYS_BIN"; else warn "kept $SYS_BIN"; fi
	fi
	rm -f "$STATE"
	if ((PURGE)); then
		rm -rf "$(dirname "$CONFIG")"
		echo "Removed $(dirname "$CONFIG")."
	else
		echo "Config kept at $CONFIG (use --purge to delete it)."
	fi
	bold "btshare uninstalled."
	exit 0
fi

# ----------------------------------------------------------------- install

self_update

[[ -z ${BTSHARE_RESET_BIN:-} ]] || state_set bin ""

OLD_BIN=$(installed_bin)
OLD_VERSION=""
[[ -z $OLD_BIN ]] || OLD_VERSION=$("$OLD_BIN" version 2>/dev/null || echo "an older version")
if [[ -n $OLD_BIN ]]; then
	bold "Updating the existing installation ($OLD_BIN, ${OLD_VERSION})"
fi

step "Checking requirements"
command -v go >/dev/null || die "Go is required to build btshare (https://go.dev/dl/)"
command -v systemctl >/dev/null || die "systemd is required"
systemctl --user show-environment >/dev/null 2>&1 || die "no systemd user session; log in to a desktop session first"
systemctl is-active --quiet bluetooth || warn "bluetooth.service is not running (sudo systemctl enable --now bluetooth)"
echo "ok"

step "Building"
VERSION=$(git -C "$SRC" describe --tags --always --dirty 2>/dev/null || echo dev)
(cd "$SRC" && go build -trimpath -ldflags "-X main.version=$VERSION" -o bin/btshare ./cmd/btshare)
echo "built btshare $VERSION"

step "Installing"
install_binary "$SRC/bin/btshare"

# --fresh: move the old config aside (not deleted, in case it's needed back)
# and forget earlier answers, so the steps below run as on a first install.
# Done only now that the new binary is built and installed.
if ((FRESH)); then
	step "Starting fresh"
	if [[ -f $CONFIG || -f $STATE ]]; then
		echo "This removes this computer's btshare setup and sets it up again:"
		[[ -f $CONFIG ]] && echo "  - $CONFIG (name, group secret, computers, shared devices)"
		[[ -f $STATE ]] && echo "  - saved installer answers (firewall, shortcut)"
		echo "Bluetooth pairings, the firewall rule and the keyboard shortcut are kept."
		confirm "Continue?" || die "cancelled; nothing was changed (pass --yes to skip this question)"
		systemctl --user stop "$UNIT" 2>/dev/null || true
		if [[ -f $CONFIG ]]; then
			backup="$CONFIG.bak-$(date +%Y%m%d-%H%M%S)"
			mv "$CONFIG" "$backup"
			rm -f "$CONFIG.lock"
			echo "Old config moved to $backup"
		fi
		bin_choice=$(state_get bin)
		rm -f "$STATE"
		[[ -z $bin_choice ]] || state_set bin "$bin_choice"
		echo "The other computers still list this one: rejoin their group with --join (or create a new one)."
	else
		echo "No previous config; nothing to remove."
	fi
fi

step "Configuration"
export BTSHARE_CONFIG="$CONFIG"
# ask_secret [hint] -> sets SECRET to the group secret typed by the user.
ask_secret() {
	interactive || die "--join needs a terminal to enter the group secret"
	echo "On the computer at $JOIN, run 'btshare invite' to see the group secret."
	read -rsp "Group secret${1:+ ($1)}: " SECRET
	echo
}

if [[ -f $CONFIG ]]; then
	"$BIN" invite >/dev/null || die "existing $CONFIG is invalid; fix it, or move it away to create a new one"
	args=()
	[[ -n $NAME ]] && args+=(-name "$NAME")
	((PORT_SET)) && args+=(-port "$PORT")
	if [[ -n $JOIN ]]; then
		ask_secret "Enter to keep the current one"
		[[ -n $SECRET ]] && args+=(-secret "$SECRET")
		unset SECRET
	fi
	if ((${#args[@]})); then
		"$BIN" reconfigure "${args[@]}"
	else
		echo "Keeping existing $CONFIG"
	fi
	PORT=$(sed -n 's/^listen = ".*:\([0-9]*\)"/\1/p' "$CONFIG")
	PORT=${PORT:-7531}
else
	args=(-port "$PORT")
	[[ -n $NAME ]] && args+=(-name "$NAME")
	if [[ -n $JOIN ]]; then
		ask_secret
		[[ -n $SECRET ]] || die "empty secret"
		args+=(-secret "$SECRET")
		unset SECRET
	fi
	"$BIN" init "${args[@]}"
fi

if ((FIREWALL)); then
	step "Firewall"
	configure_firewall open "$PORT"
fi

step "Device access"
configure_udev

step "Starting the agent"
install -Dm644 "$SRC/contrib/$UNIT" "$UNIT_DIR/$UNIT"
sed -i "s|^ExecStart=.*|ExecStart=$BIN agent|" "$UNIT_DIR/$UNIT"
systemctl --user daemon-reload
systemctl --user reset-failed "$UNIT" 2>/dev/null || true
systemctl --user enable "$UNIT" >/dev/null 2>&1
systemctl --user restart "$UNIT"
for _ in $(seq 20); do
	if (exec 3<>"/dev/tcp/127.0.0.1/$PORT") 2>/dev/null; then break; fi
	sleep 0.25
done
if systemctl --user is-active --quiet "$UNIT"; then
	echo "agent running $("$BIN" version) (logs: journalctl --user -u $UNIT -f)"
else
	journalctl --user -u "$UNIT" -n 10 --no-pager >&2 || true
	die "agent failed to start"
fi
if command -v loginctl >/dev/null && [[ $(loginctl show-user "$USER" -p Linger --value 2>/dev/null) != yes ]]; then
	echo "Note: the agent runs while you're logged in. To keep it running when logged out: loginctl enable-linger"
fi

if [[ -n $JOIN ]]; then
	step "Joining $JOIN"
	"$BIN" join "$JOIN" || die "could not join $JOIN. Is its agent running, is its firewall open, and is the secret right?"
fi

if ((HOTKEY)); then
	step "Keyboard shortcut"
	configure_hotkey
fi

step "Done"
"$BIN" status 2>/dev/null || true
cat <<EOF

Next steps (details: "Share a device" in $SRC/README.md):
  • Add another computer:  btshare invite   (shows the command to run there)
  • Share a device:        btshare device add mouse "MX Vertical"   (btshare device list shows paired devices)
                           It tells you whether the device switches channels or needs its pairing copied.
  • Update later:          re-run $SRC/install.sh
EOF

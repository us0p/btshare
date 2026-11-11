#!/usr/bin/env bash
# btshare installer. Run as your normal user; sudo is only requested (with
# your confirmation) to open the firewall port.
#
#   ./install.sh                     first computer: creates a new group
#   ./install.sh --join 10.0.0.5     other computers: join the group of 10.0.0.5
#   ./install.sh --uninstall [--purge]
set -euo pipefail

SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PREFIX="${PREFIX:-$HOME/.local}"
BIN="$PREFIX/bin/btshare"
CONFIG="${BTSHARE_CONFIG:-${XDG_CONFIG_HOME:-$HOME/.config}/btshare/config.toml}"
UNIT_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
UNIT="btshare-agent.service"
HOTKEY_PATH="/org/gnome/settings-daemon/plugins/media-keys/custom-keybindings/btshare/"
MK="org.gnome.settings-daemon.plugins.media-keys"

JOIN="" NAME="" PORT="7531" FIREWALL=1 HOTKEY=1 UNINSTALL=0 PURGE=0 ASSUME_YES=0

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
	sed -n '2,8p' "$0" | sed 's/^# \{0,1\}//'
	cat <<EOF

Options:
  --join ADDR      join the group of the computer at ADDR (ip or ip:port)
  --name NAME      this computer's name in the group (default: hostname)
  --port PORT      agent port (default 7531)
  --no-firewall    don't offer to open the firewall port
  --no-hotkey      don't offer to create the GNOME keyboard shortcut
  --yes            answer yes to every confirmation (sudo may still ask for your password)
  --uninstall      remove btshare (keeps config unless --purge)
EOF
}

while (($#)); do
	case $1 in
	--join) JOIN=${2:?--join needs an address}; shift ;;
	--name) NAME=${2:?--name needs a value}; shift ;;
	--port) PORT=${2:?--port needs a value}; shift ;;
	--no-firewall) FIREWALL=0 ;;
	--no-hotkey) HOTKEY=0 ;;
	--yes | -y) ASSUME_YES=1 ;;
	--uninstall) UNINSTALL=1 ;;
	--purge) PURGE=1 ;;
	-h | --help) usage; exit 0 ;;
	*) die "unknown option $1 (see --help)" ;;
	esac
	shift
done

[[ $EUID -ne 0 ]] || die "run as your normal user, not root; sudo is requested only when needed"
[[ $PORT =~ ^[0-9]+$ ]] && ((PORT > 0 && PORT < 65536)) || die "invalid port $PORT"

# ---------------------------------------------------------------- firewall

firewall_kind() {
	if command -v ufw >/dev/null && grep -qs '^ENABLED=yes' /etc/ufw/ufw.conf; then
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
	local action=$1 port=$2 ip subnet cmds
	read -r ip subnet < <("$BIN" net-info) || { warn "could not detect LAN subnet; skipping firewall"; return; }
	if [[ -z $(firewall_kind) ]]; then
		echo "No active ufw or firewalld found, so there's nothing to $action."
		echo "If you filter traffic with your own nftables/iptables rules, allow TCP $port from $subnet."
		return
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
	else
		warn "skipped; run the command(s) above yourself, or other computers can't reach this one"
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
	gnome_hotkeys_available || {
		echo "GNOME not detected. Bind this command to a key in your desktop's settings:"
		echo "    $BIN -notify next"
		return
	}
	local schema="$MK.custom-keybinding:$HOTKEY_PATH"
	if [[ $(gsettings get "$schema" command 2>/dev/null) == *btshare* ]]; then
		echo "Keyboard shortcut already set: $(gsettings get "$schema" binding)"
		return
	fi
	if confirm "Create the keyboard shortcut Super+Alt+B to move shared devices to the next computer?"; then
		gsettings set "$schema" name 'btshare: move devices to next computer'
		gsettings set "$schema" command "$BIN -notify next"
		gsettings set "$schema" binding '<Super><Alt>b'
		hotkey_list_edit add
		echo "Shortcut created (change it in Settings → Keyboard → Custom Shortcuts)."
	else
		echo "Skipped. Command to bind later: $BIN -notify next"
	fi
}

# --------------------------------------------------------------- uninstall

if ((UNINSTALL)); then
	step "Stopping the agent"
	systemctl --user disable --now "$UNIT" 2>/dev/null || true
	rm -f "$UNIT_DIR/$UNIT"
	systemctl --user daemon-reload
	if gnome_hotkeys_available && [[ $(gsettings get "$MK.custom-keybinding:$HOTKEY_PATH" command 2>/dev/null) == *btshare* ]]; then
		step "Removing keyboard shortcut"
		gsettings reset-recursively "$MK.custom-keybinding:$HOTKEY_PATH"
		hotkey_list_edit remove
	fi
	if [[ -x $BIN ]]; then
		port=$(sed -n 's/^listen = ".*:\([0-9]*\)"/\1/p' "$CONFIG" 2>/dev/null || true)
		step "Firewall"
		configure_firewall close "${port:-$PORT}"
		rm -f "$BIN"
	fi
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

step "Checking requirements"
command -v go >/dev/null || die "Go is required to build btshare (https://go.dev/dl/)"
command -v systemctl >/dev/null || die "systemd is required"
systemctl --user show-environment >/dev/null 2>&1 || die "no systemd user session; log in to a desktop session first"
systemctl is-active --quiet bluetooth || warn "bluetooth.service is not running (sudo systemctl enable --now bluetooth)"
echo "ok"

step "Building"
(cd "$SRC" && go build -trimpath -o bin/btshare ./cmd/btshare)
install -Dm755 "$SRC/bin/btshare" "$BIN"
echo "installed $BIN"
case ":$PATH:" in *":$PREFIX/bin:"*) ;; *) warn "$PREFIX/bin is not in your PATH; add it to use 'btshare' directly" ;; esac

step "Configuration"
export BTSHARE_CONFIG="$CONFIG"
if [[ -f $CONFIG ]]; then
	echo "Keeping existing $CONFIG"
	PORT=$(sed -n 's/^listen = ".*:\([0-9]*\)"/\1/p' "$CONFIG")
	PORT=${PORT:-7531}
else
	args=(-port "$PORT")
	[[ -n $NAME ]] && args+=(-name "$NAME")
	if [[ -n $JOIN ]]; then
		interactive || die "--join needs a terminal to enter the group secret"
		echo "On the computer at $JOIN, run 'btshare invite' to see the group secret."
		read -rsp "Group secret: " SECRET
		echo
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

step "Starting the agent"
install -Dm644 "$SRC/contrib/$UNIT" "$UNIT_DIR/$UNIT"
sed -i "s|^ExecStart=.*|ExecStart=$BIN agent|" "$UNIT_DIR/$UNIT"
systemctl --user daemon-reload
systemctl --user enable "$UNIT" >/dev/null 2>&1
systemctl --user restart "$UNIT"
for _ in $(seq 20); do
	if (exec 3<>"/dev/tcp/127.0.0.1/$PORT") 2>/dev/null; then break; fi
	sleep 0.25
done
if systemctl --user is-active --quiet "$UNIT"; then
	echo "agent running (logs: journalctl --user -u $UNIT -f)"
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

Next steps:
  • Share a device:        btshare device add mouse "MX Vertical"   (btshare device list shows paired devices)
  • Copy its pairing to the other computers: see "Pairing" in $SRC/README.md
  • Add another computer:  btshare invite
EOF

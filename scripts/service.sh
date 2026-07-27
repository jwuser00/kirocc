#!/usr/bin/env bash
#
# Manage kirocc as a macOS launchd user agent.
#
#   ./scripts/service.sh install      build, install and start the agent
#   ./scripts/service.sh uninstall    stop and remove the agent
#   ./scripts/service.sh restart      restart the running agent
#   ./scripts/service.sh status       show whether the agent is running
#   ./scripts/service.sh logs         tail the kirocc log
#
# install accepts --with-shell-env to append the Claude Code environment
# variables to your shell rc file. Those variables are read by the `claude`
# client, not by the kirocc server, so they cannot live in the launchd plist.
set -euo pipefail

LABEL="com.kirocc.server"
PORT="${KIROCC_PORT:-3456}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PLIST_TEMPLATE="$REPO_ROOT/scripts/$LABEL.plist.template"
PLIST_DEST="$HOME/Library/LaunchAgents/$LABEL.plist"
LOG_DIR="$HOME/Library/Logs/kirocc"
LOG_FILE="$LOG_DIR/kirocc.log"
LAUNCHD_LOG="$LOG_DIR/launchd.log"
INSTALL_DIR="$HOME/.local/bin"
BINARY="$INSTALL_DIR/kirocc"

# Marker so the shell rc block can be found again on uninstall.
RC_MARKER="# >>> kirocc (Claude Code proxy) >>>"
RC_MARKER_END="# <<< kirocc (Claude Code proxy) <<<"

die() {
	echo "error: $*" >&2
	exit 1
}

info() { echo "==> $*"; }

require_macos() {
	[[ "$(uname -s)" == "Darwin" ]] ||
		die "this script targets macOS launchd; on Linux use a systemd user unit (see README)"
}

domain() { echo "gui/$(id -u)"; }

is_loaded() {
	launchctl print "$(domain)/$LABEL" >/dev/null 2>&1
}

build_binary() {
	command -v go >/dev/null 2>&1 || die "go is not installed (need Go 1.26.1+)"
	info "building kirocc -> $BINARY"
	mkdir -p "$INSTALL_DIR"
	# GOEXPERIMENT=jsonv2 is mandatory: the project imports encoding/json/v2.
	( cd "$REPO_ROOT" && GOEXPERIMENT=jsonv2 go build -o "$BINARY" ./cmd/kirocc )
}

port_in_use_by_other() {
	local pids
	pids="$(lsof -nP -iTCP:"$PORT" -sTCP:LISTEN -t 2>/dev/null || true)"
	[[ -n "$pids" ]] || return 1
	# Ignore our own agent; anything else is a genuine conflict.
	local p
	for p in $pids; do
		[[ "$(ps -o comm= -p "$p" 2>/dev/null)" == "$BINARY" ]] || return 0
	done
	return 1
}

render_plist() {
	mkdir -p "$(dirname "$PLIST_DEST")" "$LOG_DIR"
	sed \
		-e "s|@LABEL@|$LABEL|g" \
		-e "s|@BINARY@|$BINARY|g" \
		-e "s|@PORT@|$PORT|g" \
		-e "s|@LOGFILE@|$LOG_FILE|g" \
		-e "s|@LAUNCHD_LOG@|$LAUNCHD_LOG|g" \
		-e "s|@HOME@|$HOME|g" \
		"$PLIST_TEMPLATE" >"$PLIST_DEST"
}

load_agent() {
	# bootstrap is the modern API; fall back to load -w on older systems.
	launchctl bootstrap "$(domain)" "$PLIST_DEST" 2>/dev/null ||
		launchctl load -w "$PLIST_DEST"
}

unload_agent() {
	launchctl bootout "$(domain)/$LABEL" 2>/dev/null ||
		launchctl unload -w "$PLIST_DEST" 2>/dev/null ||
		true
}

shell_rc() {
	case "${SHELL##*/}" in
	zsh) echo "$HOME/.zshrc" ;;
	bash) echo "$HOME/.bash_profile" ;;
	*) echo "" ;;
	esac
}

install_shell_env() {
	local rc
	rc="$(shell_rc)"
	if [[ -z "$rc" ]]; then
		echo "could not detect your shell rc file; add these lines manually:" >&2
		print_shell_env
		return
	fi
	if [[ -f "$rc" ]] && grep -qF "$RC_MARKER" "$rc"; then
		info "shell env already present in $rc, leaving it alone"
		return
	fi
	info "appending Claude Code env to $rc"
	{
		echo ""
		echo "$RC_MARKER"
		echo "export ANTHROPIC_BASE_URL=http://127.0.0.1:$PORT"
		echo "export ANTHROPIC_AUTH_TOKEN=dummy"
		echo "$RC_MARKER_END"
	} >>"$rc"
	echo "   open a new terminal (or: source $rc) for it to take effect"
}

remove_shell_env() {
	local rc
	rc="$(shell_rc)"
	[[ -n "$rc" && -f "$rc" ]] || return 0
	grep -qF "$RC_MARKER" "$rc" || return 0
	info "removing kirocc env block from $rc"
	local tmp
	tmp="$(mktemp)"
	sed "/^${RC_MARKER}$/,/^${RC_MARKER_END}$/d" "$rc" >"$tmp"
	mv "$tmp" "$rc"
}

print_shell_env() {
	cat <<EOF

  export ANTHROPIC_BASE_URL=http://127.0.0.1:$PORT
  export ANTHROPIC_AUTH_TOKEN=dummy
EOF
}

cmd_install() {
	require_macos
	local with_shell_env=0
	[[ "${1:-}" == "--with-shell-env" ]] && with_shell_env=1

	build_binary

	if is_loaded; then
		info "agent already loaded, replacing it"
		unload_agent
	elif port_in_use_by_other; then
		die "port $PORT is already in use by another process; stop it or set KIROCC_PORT"
	fi

	render_plist
	info "loading $LABEL"
	load_agent

	# Give launchd a moment, then confirm the port actually answers.
	local i
	for i in $(seq 1 20); do
		if curl -fsS --max-time 2 "http://127.0.0.1:$PORT/health" >/dev/null 2>&1; then
			info "kirocc is serving on http://127.0.0.1:$PORT"
			break
		fi
		[[ $i -eq 20 ]] && die "agent loaded but /health never answered; check $LAUNCHD_LOG and $LOG_FILE"
		sleep 0.5
	done

	if [[ $with_shell_env -eq 1 ]]; then
		install_shell_env
	else
		echo ""
		echo "Add these to your shell rc so Claude Code talks to kirocc"
		echo "(or re-run with --with-shell-env to do it automatically):"
		print_shell_env
	fi
}

cmd_uninstall() {
	require_macos
	if is_loaded; then
		info "stopping $LABEL"
		unload_agent
	else
		info "agent is not loaded"
	fi
	[[ -f "$PLIST_DEST" ]] && { info "removing $PLIST_DEST"; rm -f "$PLIST_DEST"; }
	remove_shell_env
	echo ""
	echo "Left in place (remove by hand if you want them gone):"
	echo "  binary : $BINARY"
	echo "  logs   : $LOG_DIR"
}

cmd_restart() {
	require_macos
	is_loaded || die "agent is not loaded; run: $0 install"
	info "restarting $LABEL"
	launchctl kickstart -k "$(domain)/$LABEL"
}

cmd_status() {
	require_macos
	if is_loaded; then
		local pid state
		pid="$(launchctl print "$(domain)/$LABEL" | awk '/^\tpid =/ {print $3}')"
		state="$(launchctl print "$(domain)/$LABEL" | awk '/^\tstate =/ {print $3}')"
		echo "agent   : loaded (state=${state:-unknown} pid=${pid:-none})"
	else
		echo "agent   : not loaded"
	fi
	echo "plist   : $PLIST_DEST"
	echo "binary  : $BINARY"
	echo "log     : $LOG_FILE"
	if curl -fsS --max-time 2 "http://127.0.0.1:$PORT/health" >/dev/null 2>&1; then
		echo "health  : ok (http://127.0.0.1:$PORT)"
	else
		echo "health  : not responding on port $PORT"
	fi
}

cmd_logs() {
	[[ -f "$LOG_FILE" ]] || die "no log yet at $LOG_FILE"
	tail -f "$LOG_FILE"
}

case "${1:-}" in
install) shift; cmd_install "${1:-}" ;;
uninstall) cmd_uninstall ;;
restart) cmd_restart ;;
status) cmd_status ;;
logs) cmd_logs ;;
*)
	sed -n '2,17p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
	exit 1
	;;
esac

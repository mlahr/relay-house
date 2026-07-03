#!/usr/bin/env bash
set -euo pipefail

readonly REPO_OWNER="mlahr"
readonly REPO_NAME="email-endpoint"
readonly RELEASE_BASE_URL="https://github.com/${REPO_OWNER}/${REPO_NAME}/releases/latest/download"
readonly INSTALLER_VERSION="2026-07-02.1"

die() {
	printf 'install.sh: %s\n' "$*" >&2
	exit 1
}

need_command() {
	command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

if [ "$(uname -s)" != "Linux" ]; then
	die "unsupported operating system: $(uname -s); this installer supports Debian-based Linux only"
fi

printf 'email-endpoint installer %s\n' "$INSTALLER_VERSION"

need_command curl
need_command dpkg
need_command apt-get
need_command sha256sum

arch="$(dpkg --print-architecture)"
case "$arch" in
	amd64)
		asset_arch="amd64"
		;;
	*)
		die "unsupported Debian architecture: $arch; current email-endpoint releases publish linux amd64 .deb assets only"
		;;
esac

if [ "$(id -u)" -eq 0 ]; then
	sudo_cmd=()
else
	need_command sudo
	sudo_cmd=(sudo)
fi

tmp_dir="$(mktemp -d)"
chmod 0755 "$tmp_dir"
cleanup() {
	rm -rf "$tmp_dir"
}
trap cleanup EXIT

checksums_file="$tmp_dir/checksums.txt"
curl -fsSL "$RELEASE_BASE_URL/checksums.txt" -o "$checksums_file"

asset_name="$(
	awk -v pattern="email-endpoint_.*_linux_${asset_arch}[.]deb$" '$2 ~ pattern { print $2 }' "$checksums_file"
)"

if [ -z "$asset_name" ]; then
	die "no linux ${asset_arch} .deb asset found in latest release checksums.txt"
fi

if [ "$(printf '%s\n' "$asset_name" | wc -l | tr -d ' ')" != "1" ]; then
	die "multiple linux ${asset_arch} .deb assets found in latest release checksums.txt"
fi

deb_file="$tmp_dir/$asset_name"
curl -fsSL "$RELEASE_BASE_URL/$asset_name" -o "$deb_file"
chmod 0644 "$deb_file"

(
	cd "$tmp_dir"
	grep -F "  $asset_name" checksums.txt | sha256sum -c -
)

if [ ! -r /dev/tty ]; then
	die "interactive package installation requires a controlling terminal"
fi

"${sudo_cmd[@]}" apt-get install -y "$deb_file" </dev/tty

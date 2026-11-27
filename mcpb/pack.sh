#!/bin/sh
# Packs the Claude Desktop extension, <out-dir>/arc-mcp-<version>.mcpb: this
# directory's manifest.json (version stamped), LICENSE, the darwin universal
# binary as server/arc-mcp and the Windows build as server/arc-mcp.exe.
#
# GoReleaser runs it as the universal binary's post hook (.goreleaser.yml),
# after every build and before archives, checksums and publishing, so a
# failure here stops the release before anything is public, and the bundle
# lands in checksums.txt. POSIX sh plus jq, zip and unzip; nothing else.
#
#   pack.sh <darwin-universal-binary> <version> <commit-epoch> [out-dir]
#
# <version> is GoReleaser's .Version: the file name keeps it as is
# (arc-mcp-26.04.3.mcpb, like every other asset), the manifest gets it as
# semver (26.4.3), which Claude Desktop expects. out-dir defaults to the
# dist directory the binary is in. The zip is reproducible: fixed mtimes
# (the commit's), fixed modes, fixed entry order, no extra fields.
set -eu
export LC_ALL=C TZ=UTC
umask 022

die() {
	echo "mcpb/pack.sh: $*" >&2
	exit 1
}

[ $# -ge 3 ] && [ $# -le 4 ] || die "usage: pack.sh <darwin-universal-binary> <version> <commit-epoch> [out-dir]"
fat=$1
version=$2
epoch=$3
for tool in jq zip unzip od; do
	command -v "$tool" >/dev/null 2>&1 || die "$tool is required"
done
here=$(cd "$(dirname "$0")" && pwd)
[ -f "$fat" ] || die "no darwin universal binary at $fat"
dist=$(cd "$(dirname "$fat")/.." && pwd)
[ -d "${4:-$dist}" ] || die "no output directory ${4:-$dist}"
out=$(cd "${4:-$dist}" && pwd)
case $epoch in '' | *[!0-9]*) die "commit epoch $epoch is not a number" ;; esac

# The manifest version: MAJOR.MINOR.PATCH without leading zeros, then the
# prerelease or build suffix as is. Anything that is not semver is refused.
core=${version%%[-+]*}
suffix=${version#"$core"}
semver=$(printf '%s\n' "$core" | awk -F. 'NF == 3 && $1 ~ /^[0-9]+$/ && $2 ~ /^[0-9]+$/ && $3 ~ /^[0-9]+$/ { printf "%d.%d.%d", $1, $2, $3; ok = 1 } END { exit !ok }') ||
	die "version $version is not MAJOR.MINOR.PATCH[-prerelease]"
semver=$semver$suffix
id='(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)'
printf '%s\n' "$semver" | grep -Eq "^[0-9]+\.[0-9]+\.[0-9]+(-$id(\.$id)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?\$" ||
	die "manifest version $semver (from $version) is not semver"

# Exactly one Windows build, next to the universal binary.
set -- "$dist"/arc-mcp_windows_amd64*/arc-mcp.exe
[ $# -eq 1 ] && [ -f "$1" ] || die "expected one Windows build under $dist, found: $*"
win=$1

# The darwin binary must be a universal (fat) Mach-O holding x86_64 and
# arm64; the Windows one a PE file.
hex() { od -An -tx1 -j "$2" -N "$3" "$1" | tr -d ' \n'; }
[ "$(hex "$fat" 0 4)" = cafebabe ] || die "$fat is not a universal Mach-O binary"
[ "$(hex "$fat" 4 4)" = 00000002 ] || die "$fat does not hold exactly two architectures"
archs=$(printf '%s\n%s\n' "$(hex "$fat" 8 4)" "$(hex "$fat" 28 4)" | sort | tr '\n' ' ')
[ "$archs" = "01000007 0100000c " ] || die "$fat architectures are $archs, not x86_64 and arm64"
[ "$(hex "$win" 0 2)" = 4d5a ] || die "$win is not a Windows executable"

name=arc-mcp-$version.mcpb
stage=$out/mcpb-stage-$version
[ ! -e "$stage" ] && [ ! -e "$out/$name" ] || die "$stage or $out/$name already exists"
mkdir "$stage" "$stage/server"
jq --arg v "$semver" '.version = $v' "$here/manifest.json" >"$stage/manifest.json"
cp "$here/../LICENSE" "$stage/LICENSE"
cp "$fat" "$stage/server/arc-mcp"
cp "$win" "$stage/server/arc-mcp.exe"
chmod 644 "$stage/manifest.json" "$stage/LICENSE"
chmod 755 "$stage/server" "$stage/server/arc-mcp" "$stage/server/arc-mcp.exe"

# Checks the mcpb 2.1.2 CLI does not make (it validates the schema only).
m=$stage/manifest.json
jq -e --arg v "$semver" '.version == $v' "$m" >/dev/null || die "manifest version not stamped"
entry=$(jq -r '.server.entry_point' "$m")
[ -x "$stage/$entry" ] || die "entry point $entry is not an executable file in the bundle"
# An override that sets env replaces the base env in mcpb 2.1.2 (mcpb#217),
# which would drop ARC_URL and ARC_TOKEN.
jq -e '[.server.mcp_config.platform_overrides // {} | .[] | has("env")] | any | not' "$m" >/dev/null ||
	die "a platform override sets env"
# A referenced field that is neither required nor defaulted reaches the
# server as the literal \${user_config.x} when left empty (mcpb#250).
loose=$(jq -r '. as $root | [.server.mcp_config | .. | strings | scan("\\$\\{user_config\\.([A-Za-z0-9_]+)\\}") | .[0]] | unique | map(select(($root.user_config[.] // {}) as $u | ($u.required != true) and ($u | has("default") | not))) | join(" ")' "$m")
[ -z "$loose" ] || die "user_config referenced without required or default: $loose"
# A sensitive value goes in env only, never on the command line (ps).
onargs=$(jq -r '. as $root | [.server.mcp_config | .command, (.args // [])[], (.platform_overrides // {} | .[] | .command // empty, (.args // [])[])] | map(scan("\\$\\{user_config\\.([A-Za-z0-9_]+)\\}") | .[0]) | unique | map(select($root.user_config[.].sensitive == true)) | join(" ")' "$m")
[ -z "$onargs" ] || die "sensitive user_config on the command line: $onargs"

# Reproducible zip: commit mtimes (TZ=UTC: zip stores local time), no extra
# fields (-X), no directory entries (-D), entries in a fixed order.
stamp=$(date -u -d "@$epoch" +%Y%m%d%H%M.%S 2>/dev/null || date -u -r "$epoch" +%Y%m%d%H%M.%S) ||
	die "cannot format commit epoch $epoch"
for f in manifest.json LICENSE server/arc-mcp server/arc-mcp.exe; do
	touch -t "$stamp" "$stage/$f"
done
tmp=$out/.$name.tmp
(cd "$stage" && zip -q -X -D "$tmp" manifest.json LICENSE server/arc-mcp server/arc-mcp.exe)

# Read it back: intact, Unix modes kept (Claude Desktop runs server/arc-mcp
# from them), the stamped manifest inside.
unzip -tq "$tmp" >/dev/null || die "$tmp fails its integrity test"
modes=$(unzip -Z "$tmp" | awk '$NF ~ /^(manifest\.json|LICENSE|server\/arc-mcp|server\/arc-mcp\.exe)$/ { printf "%s %s %s;", $NF, $1, $3 }')
[ "$modes" = "manifest.json -rw-r--r-- unx;LICENSE -rw-r--r-- unx;server/arc-mcp -rwxr-xr-x unx;server/arc-mcp.exe -rwxr-xr-x unx;" ] ||
	die "unexpected entries or modes in $tmp: $modes"
[ "$(unzip -p "$tmp" manifest.json | jq -r .version)" = "$semver" ] || die "the bundled manifest does not say $semver"
mv "$tmp" "$out/$name"
echo "mcpb/pack.sh: $out/$name (manifest version $semver)"

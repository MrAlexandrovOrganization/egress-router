#!/usr/bin/env bash
set -euo pipefail
umask 077
# Called under the deployment lock, with sudo, from the repository root.
uid=${1:-1000}
gid=${2:-1000}
[[ $uid =~ ^[0-9]+$ && $gid =~ ^[0-9]+$ ]] || exit 2
for dir in state runtime; do
    [[ ! -L $dir ]] || { printf 'Refusing symlink directory\n' >&2; exit 1; }
    install -d -m 0700 -o "$uid" -g "$gid" "$dir"
done
for file in config.json state/subscriptions.json runtime/config.json runtime/active.json runtime/previous.json; do
    [[ ! -L $file && ( ! -e $file || -f $file ) ]] || {
        printf 'Expected regular deployment files, not directories or symlinks\n' >&2; exit 1;
    }
done
[[ -f config.json ]] || install -m 0600 config.json.example config.json
if [[ ! -e state/subscriptions.json ]]; then
    source=subscription-manager/subscriptions.json.example
    if [[ -e subscription-manager/subscriptions.json ]]; then
        [[ -f subscription-manager/subscriptions.json && ! -L subscription-manager/subscriptions.json ]] || exit 1
        source=subscription-manager/subscriptions.json
    fi
    install -m 0600 -o "$uid" -g "$gid" "$source" state/.subscriptions.new
    mv state/.subscriptions.new state/subscriptions.json
fi
chown "$uid:$gid" config.json state/subscriptions.json
chmod 0600 config.json state/subscriptions.json
[[ -f runtime/config.json ]] || install -m 0640 config.json runtime/config.json
[[ -f runtime/active.json ]] || install -m 0640 runtime/config.json runtime/active.json
chown "$uid:$gid" runtime/config.json runtime/active.json
chmod 0640 runtime/config.json runtime/active.json

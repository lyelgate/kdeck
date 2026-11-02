#!/bin/sh
# Writes shell completion scripts to completions/ for the release packages.
set -eu
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
go build -o "$TMP/kdeck" ./cmd/kdeck
mkdir -p completions
for sh in bash zsh fish; do
  KDECK_HOME="$TMP/home" "$TMP/kdeck" completion "$sh" > "completions/kdeck.$sh"
done

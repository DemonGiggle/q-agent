#!/bin/sh
set -eu

if version=$(git describe --tags --exact-match HEAD 2>/dev/null); then
	printf '%s\n' "$version"
else
	printf '%s\n' dev
fi

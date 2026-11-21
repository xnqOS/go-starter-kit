#!/usr/bin/env sh
set -eu

if [ "$#" -eq 0 ] || [ -z "$1" ]; then
    echo 'Usage: ./scripts/add-module.sh <module_name> [-path /route] [-dry-run]' >&2
    exit 1
fi

module_name=$1
shift
project_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$project_root"
exec go run ./cmd/modulegen -name "$module_name" "$@"

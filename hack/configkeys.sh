#!/usr/bin/env bash
# configkeys.sh — the configuration key budget (plan §6): the leaf keys of
# chok.Base and of every battery's Config, against limit config_keys in
# hack/budget.txt. `make budget` runs it after apibudget.sh.
set -euo pipefail
cd "$(dirname "$0")/.."
go run ./internal/tools/configkeys hack/budget.txt

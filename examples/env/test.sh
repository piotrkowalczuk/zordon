#!/usr/bin/env bash
# Claim: env precedence — process < dotenv < env{}. The env{} block's
# OVERRIDE_ME must win over the dotenv's; both static and dynamic
# (port-interpolated) values are injected.
cd "$(dirname "$0")"
source ../_lib.sh
need curl

start
port="$(port_of "/app -addr")" || fail "could not discover app port"
env="$(http_get "http://127.0.0.1:$port/env")" || fail "/env not responding"
assert_contains "$env" "ENV_STATIC=hello"          "static env value"
assert_contains "$env" "DOTENV_FROM_FILE=1"        "dotenv value injected"
assert_contains "$env" "OVERRIDE_ME=from-env"      "env{} overrides dotenv"
assert_contains "$env" "ENV_DYN=127.0.0.1:$port"   "dynamic env value"

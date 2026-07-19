#!/usr/bin/env bash
# Claim: a Java (Spring Boot) service cloned from git, built with its
# committed Maven wrapper (./mvnw), comes up and serves its home page —
# proving git source + wrapper-first default build + jar run-inference.
cd "$(dirname "$0")"
source ../_lib.sh
need git
need curl
need_net

# PetClinic is a real Spring Boot app: git clone + `./mvnw package`
# (downloads Maven + hundreds of deps on a cold cache) + JVM boot takes
# minutes, so raise the bringup timeout well past the harness's 90s
# default. The later --timeout wins (ff last-value-wins).
start --timeout 900s

# The inferred `java -jar` run carries no -addr argv (the port travels via
# SERVER_PORT env), so discover it from the running stack, not from ps.
port="$(zordon get service.java.petclinic.vars.port)" || fail "could not read petclinic port"
[ -n "$port" ] || fail "empty petclinic port"

body="$(http_get "http://127.0.0.1:$port/")" || fail "petclinic home not responding on :$port"
assert_contains "$body" "PetClinic" "petclinic home page"

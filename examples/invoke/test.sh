#!/usr/bin/env bash
# Claim: a latent provision (`after = never`) never auto-runs, but two
# peers each invoke it for themselves via `cmd = <provision ref>` with
# their own env. The shared topics file ends up with exactly the two
# consumer topics — proof of N independent invocations and no auto-run.
cd "$(dirname "$0")"
source ../_lib.sh
need curl

start

# Both consumers expose an http endpoint; reaching them confirms the
# stack came up (provisions default to after = [self.runtime.ready]).
app_port="$(port_of "/app -addr")" || fail "could not discover app port"
http_get "http://127.0.0.1:$app_port/" >/dev/null || fail "app not responding"

topics="$(zordon get service.go.kafka.vars.topics)" || fail "get topics path failed"
[ -r "$topics" ] || fail "topics file not created at $topics — invocations did not run"

sorted="$(sort "$topics")"
want="$(printf 'app-events\nbilling-events\n')"
if [ "$sorted" = "$want" ]; then
	pass "topics file == {app-events, billing-events} (two independent invocations)"
else
	fail "topics file = $(tr '\n' ',' <"$topics"); want app-events,billing-events (a 3rd/empty line = latent auto-ran)"
fi

lines="$(wc -l <"$topics" | tr -d ' ')"
[ "$lines" = "2" ] && pass "exactly 2 lines (latent create-topic did not auto-run)" \
	|| fail "expected 2 lines, got $lines (latent provision auto-ran?)"

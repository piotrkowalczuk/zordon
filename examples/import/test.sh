#!/usr/bin/env bash
# Claim: an entrypoint composes modules imported from fragment files, and
# modules require each other. The app and billing modules each require kafka;
# the entrypoint imports only app and billing. kafka is loaded once and runs
# once, both consumers invoke its provision across files, relative src paths
# anchor to the declaring fragment, and a module declared but never imported
# stays out of the stack.
cd "$(dirname "$0")"
source ../_lib.sh
need curl

start

plan="$(zordon --agent plan)" || fail "zordon plan failed"
assert_contains "$plan" "# import $EXROOT/services/apps/Alphasfile.apps [app, billing]" "plan lists the apps import"
assert_contains "$plan" "# import $EXROOT/services/kafka/Alphasfile.kafka [kafka]" "plan lists the transitive kafka import"
assert_contains "$plan" "# unused module kafka-ui in $EXROOT/services/kafka/Alphasfile.kafka" "plan names the never-imported module"
n="$(printf '%s\n' "$plan" | grep -c '^# import .*Alphasfile.kafka')"
[ "$n" = "1" ] && pass "kafka's fragment is loaded once" || fail "kafka's fragment listed $n times"

for svc in app/app billing/billing kafka/kafka; do
	zordon status --agent 2>/dev/null | grep -q "$svc" || fail "$svc missing from zordon status"
done
pass "app/app, billing/billing and kafka/kafka are part of the stack"
if zordon status --agent 2>/dev/null | grep -q "kafka-ui/ui"; then
	fail "kafka-ui/ui must not run: nothing imports module kafka-ui"
fi
pass "the unimported kafka-ui module stayed out"

app_port="$(zordon get module.app.service.go.app.vars.port)" || fail "get app port failed"
http_get "http://127.0.0.1:$app_port/" >/dev/null || fail "app not responding"
pass "app serves on $app_port"

dir="$(zordon get module.kafka.service.go.kafka.dir)" || fail "get kafka dir failed"
case "$dir" in
	*/examples/import/src/kafka) pass "kafka's relative src path anchored to its fragment ($dir)" ;;
	*) fail "kafka dir = $dir, want .../examples/import/src/kafka" ;;
esac

topics="$(zordon get module.kafka.service.go.kafka.vars.topics)" || fail "get topics path failed"
for i in $(seq 1 50); do
	[ "$(wc -l <"$topics" 2>/dev/null | tr -d ' ')" = "2" ] && break
	sleep 0.2
done
sorted="$(sort "$topics")"
want="$(printf 'app-events\nbilling-events\n')"
[ "$sorted" = "$want" ] \
	&& pass "both consumers invoked kafka's provision across files" \
	|| fail "topics file = $(tr '\n' ',' <"$topics"); want app-events,billing-events"

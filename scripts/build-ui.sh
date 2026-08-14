#!/usr/bin/env bash
# Check the operator interface.
#
# There is no bundler and no dependency to install: the interface is ES modules
# the browser loads directly. What this does is what a build step would
# otherwise hide — parse every module, run the unit tests, and refuse anything
# that would fail in a browser at a cashier's desk.
set -euo pipefail

cd "$(dirname "$0")/.."

if ! command -v node >/dev/null 2>&1; then
	echo "node is required to check the UI (nothing is compiled; it parses and tests)"
	exit 1
fi

echo "parsing modules..."
for module in web/*.js; do
	node --check "$module"
	echo "  ok $module"
done

echo "running UI tests..."
node --test web/test/*.test.js

# The interface must stay self-contained: a CDN reference is a screen that
# stops working when the campus link does, which at a cashier's desk means the
# queue stops.
echo "checking for external references..."
if grep -nE '(src|href)="https?://' web/index.html web/*.js 2>/dev/null; then
	echo "the interface must not load anything from outside this deployment"
	exit 1
fi

echo "ui: ok"

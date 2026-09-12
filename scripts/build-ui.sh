#!/usr/bin/env bash
# Build and check the operator interface.
#
# The interface is React and TypeScript built by Vite, and the output is
# embedded into the binary by webui/embed.go — so this has to run before
# `go build` if the served interface is to match the source.
#
# What is checked here is what a build alone would let through:
#
#   - the money tests, including the tafqit vectors generated from
#     internal/domain/money. The Arabic spelling of an amount exists in two
#     implementations, and this is what keeps them from disagreeing in an
#     operator's hands.
#   - that nothing is loaded from outside the deployment. A CDN reference is a
#     screen that stops working when the campus link does, which at a window
#     means the queue stops.
set -euo pipefail

cd "$(dirname "$0")/.."

if ! command -v node >/dev/null 2>&1; then
	echo "node is required to build the operator interface"
	exit 1
fi

cd webui

if [ ! -d node_modules ]; then
	echo "installing dependencies..."
	npm ci --no-audit --no-fund 2>/dev/null || npm install --no-audit --no-fund
fi

echo "typechecking..."
npx tsc --noEmit

echo "running unit tests..."
npx vitest run --reporter=dot

echo "building..."
npx vite build

# The bundle must be self-contained. Vite inlines nothing external by default,
# but a hand-written URL in a component would slip through the build happily.
echo "checking for external references..."
if grep -rnE '(src|href)="https?://' dist/index.html 2>/dev/null; then
	echo "the interface must not load anything from outside this deployment"
	exit 1
fi
if grep -rlE 'https?://(cdn|fonts|unpkg|jsdelivr)' dist/assets/ 2>/dev/null; then
	echo "the interface must not reference a CDN"
	exit 1
fi

echo "ui: ok"

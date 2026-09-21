#!/usr/bin/env bash
# Regenerate the Arabic spelling vectors the interface is tested against.
#
# التفقيط exists twice on purpose: internal/domain/money renders it for the
# printed receipt, and webui renders it on the confirmation sheet, where there
# is no receipt yet because nothing has been posted. Two implementations of an
# intricate grammar is a real risk, and this file is what makes it acceptable —
# every vector below comes out of the Go implementation, so a divergence in the
# port fails a test rather than reaching a cashier as a screen and a slip that
# disagree.
#
# Run after any change to internal/domain/money/arabic.go.
set -euo pipefail

cd "$(dirname "$0")/.."

out=webui/src/lib/__fixtures__/tafqit.golden.json
gen=$(mktemp -d)
trap 'rm -rf "$gen"' EXIT

# The generator lives inside the module for the duration of the run: the money
# package is internal, so a program outside the module cannot import it.
work=.tafqitgen
rm -rf "$work"
mkdir -p "$work"
trap 'rm -rf "$gen" "$work"' EXIT

cat > "$work/main.go" <<'EOF'
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"flowed/internal/domain/money"
)

func main() {
	var cases []int64
	// Every boundary the grammar turns on: the ones and teens, the tens, the
	// round hundred that returns to the singular, and the duals.
	for i := int64(0); i <= 120; i++ {
		cases = append(cases, i)
	}
	for _, n := range []int64{
		199, 200, 201, 202, 203, 210, 211, 212, 220, 300, 500, 900, 999,
		1000, 1001, 1002, 1003, 1010, 1011, 1100, 2000, 2001, 3000, 10000,
		11000, 12000, 20000, 21000, 22000, 100000, 200000, 300000, 500000,
		1000000, 1500000, 2000000, 2100000, 3000000, 11000000, 1247300000,
		1000000000, 2000000000, 700000, 450000, 1200000, 1700000, 800000,
		130000, 250000, 60000, 40000, 400000, 600000, 917, 1204,
		999999999, 1000000000000, 2000000000000,
	} {
		cases = append(cases, n)
	}
	for _, n := range []int64{-1, -2, -250000, -400000, -900000} {
		cases = append(cases, n)
	}

	out := make([]map[string]any, 0, len(cases))
	for _, n := range cases {
		a := money.Amount(n)
		out = append(out, map[string]any{
			"n":     fmt.Sprint(n),
			"spell": money.SpellArabic(a),
			"plain": money.SpellArabicPlain(a),
			"fmt":   money.FormatWesternDigits(a),
		})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		panic(err)
	}
}
EOF

mkdir -p "$(dirname "$out")"
go run "./$work" > "$out"

echo "wrote $out ($(grep -c '"n":' "$out") vectors)"

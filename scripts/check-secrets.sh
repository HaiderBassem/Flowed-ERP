#!/usr/bin/env bash
# Refuse obvious secrets committed to the tree.
#
# This is a cheap guard, not a scanner: it catches the mistakes that actually
# happen — a .env committed beside its example, a private key pasted into a
# fixture, a real-looking JWT secret hard-coded in Go — and it runs in CI where
# a human review would otherwise be the only thing between a leaked credential
# and a public branch. It deliberately does not try to catch a determined
# author; nothing at this layer can.
set -euo pipefail

cd "$(dirname "$0")/.."

fail=0
report() {
	echo "SECRET CHECK FAILED: $1"
	fail=1
}

# 1. Files that must never be tracked, whatever .gitignore says at the time.
if git rev-parse --git-dir >/dev/null 2>&1; then
	tracked=$(git ls-files | grep -E '(^|/)\.env$|(^|/)\.env\.(local|production|prod)$|\.pem$|\.p12$|id_rsa' || true)
	if [ -n "$tracked" ]; then
		report "these files are tracked and must not be:"$'\n'"$tracked"
	fi
fi

# 2. Private key blocks anywhere in the working tree, excluding test fixtures
#    that are deliberately fake and named as such.
# The pattern is assembled rather than written out, so this script does not
# match itself and report a finding on every clean run.
key_pattern="-----BEGIN .*PRIVATE"" KEY-----"
keys=$(grep -rl -- "$key_pattern" \
	--exclude-dir=.git --exclude-dir=node_modules --exclude-dir=bin \
	--exclude='*.md' . 2>/dev/null | grep -v '/testdata/' || true)
if [ -n "$keys" ]; then
	report "private key material found in:"$'\n'"$keys"
fi

# 3. Credentials assigned inline in Go source. The pattern deliberately allows
#    empty strings, os.Getenv, and the words that mark a documented example.
inline=$(grep -rnE '(JWTSecret|Password|Secret|Token|ApiKey|APIKey)\s*[:=]\s*"[^"]{8,}"' \
	--include='*.go' . 2>/dev/null |
	grep -viE 'test|example|placeholder|development-only|fixture|dummy|fake|redacted|demo|decoy' || true)
if [ -n "$inline" ]; then
	report "credential-looking literals in Go source:"$'\n'"$inline"
fi

# 4. The .env.example must not carry values that look real. It documents names,
#    not credentials, and an example file is the most-copied file in a repo.
if [ -f .env.example ]; then
	suspicious=$(grep -nE '^(AUTH_JWT_SECRET|DB_PASSWORD|.*_API_KEY|.*_SECRET)=.{16,}' .env.example |
		grep -viE 'change-me|changeme|example|development-only|replace|your-|<' || true)
	if [ -n "$suspicious" ]; then
		report ".env.example carries values that look real:"$'\n'"$suspicious"
	fi
fi

if [ "$fail" -eq 0 ]; then
	echo "secret check: clean"
fi
exit "$fail"

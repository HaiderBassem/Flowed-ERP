package buildinfo

import (
	"strings"
	"testing"
	"time"
)

func TestIsIdentifiedRejectsPlaceholders(t *testing.T) {
	// "dev" is rejected explicitly. It was the old fallback, so a binary built
	// by an out-of-date pipeline would otherwise pass the production check
	// while carrying exactly the string the check exists to catch.
	for _, v := range []string{Unknown, "", "dev"} {
		if (Info{Version: v}).IsIdentified() {
			t.Errorf("version %q must not count as identified", v)
		}
	}
	for _, v := range []string{"1.0.0", "1.2.3-rc1", "v2.0.0"} {
		if !(Info{Version: v}).IsIdentified() {
			t.Errorf("version %q should count as identified", v)
		}
	}
}

func TestResolveFallsBackToUnknownNotToAPlausibleString(t *testing.T) {
	got := resolve()
	if got.Version == "" {
		t.Fatal("resolve must never return an empty version")
	}
	if got.GoVersion == "" {
		t.Fatal("go version should always be available")
	}
	// Built by `go test` inside the repository: Go stamps the revision, so the
	// commit is known even though no ldflags were passed.
	if got.Commit == "" {
		t.Error("commit should fall back to the VCS revision Go embeds")
	}
}

func TestStringRendersIdentity(t *testing.T) {
	info := Info{Version: "1.0.0", Commit: "a1b2c3d4e5f6"}
	if got := info.String(); got != "1.0.0+a1b2c3d4e5f6" {
		t.Errorf("String() = %q", got)
	}
	info.Dirty = true
	if got := info.String(); !strings.HasSuffix(got, "(dirty)") {
		t.Errorf("a dirty build must say so: %q", got)
	}
	bare := Info{Version: "1.0.0", Commit: Unknown}
	if got := bare.String(); got != "1.0.0" {
		t.Errorf("an unknown commit should be omitted, got %q", got)
	}
}

func TestShortCommitTrims(t *testing.T) {
	if got := shortCommit("0123456789abcdef0123"); got != "0123456789ab" {
		t.Errorf("shortCommit = %q", got)
	}
	if got := shortCommit("abc"); got != "abc" {
		t.Errorf("a short value should pass through, got %q", got)
	}
}

func TestAge(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	info := Info{BuildTime: "2026-08-14T09:00:00Z"}
	age, ok := info.Age(now)
	if !ok || age != 3*time.Hour {
		t.Fatalf("Age() = %v, %t", age, ok)
	}

	if _, ok := (Info{}).Age(now); ok {
		t.Error("an unset build time must report unknown, not zero age")
	}
	if _, ok := (Info{BuildTime: "not a time"}).Age(now); ok {
		t.Error("an unparseable build time must report unknown")
	}
}

func TestDescribeNamesEveryField(t *testing.T) {
	got := Info{Version: "1.0.0", Commit: "abc", GoVersion: "go1.26"}.Describe()
	for _, want := range []string{"version=1.0.0", "commit=abc", "built=unknown", "dirty=false", "go=go1.26"} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe() = %q, missing %q", got, want)
		}
	}
}

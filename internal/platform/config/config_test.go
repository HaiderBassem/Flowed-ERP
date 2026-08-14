package config_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/swibit/flowed/internal/platform/config"
)

// baseEnv is the minimum a Load call needs to succeed.
func baseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("APP_ENV", "development")
	t.Setenv("DB_NAME", "flowed_test")
}

// TestEveryDocumentedVariableIsRead is the guard for the mistake that produced
// this test: two configuration fields were declared, documented, defaulted in
// the struct, and never read by Load. The features behind them — per-account
// lockout and immediate session revocation — silently did nothing, and both
// looked correct in code review. Neither had a test that could tell.
//
// The check is deliberately mechanical: every environment variable named in
// this package's source must appear inside the Load function. It cannot prove
// the value lands in the right field, but it does prove nobody added a variable
// and forgot to wire it, which is the failure that actually happened.
func TestEveryDocumentedVariableIsRead(t *testing.T) {
	source, err := os.ReadFile(filepath.Join(".", "config.go"))
	if err != nil {
		t.Fatalf("reading config.go: %v", err)
	}
	text := string(source)

	loadStart := strings.Index(text, "func Load()")
	if loadStart < 0 {
		t.Fatal("cannot find func Load in config.go")
	}
	// Load ends where the validation function begins; everything between is
	// where variables are read.
	loadEnd := strings.Index(text[loadStart:], "\nfunc ")
	loadBody := text[loadStart:]
	if loadEnd > 0 {
		loadBody = text[loadStart : loadStart+loadEnd]
	}

	// Names look like "APP_ENV" or "AUTH_JWT_SECRET": upper-case, underscored,
	// at least two segments, so ordinary constants are not swept up.
	namePattern := regexp.MustCompile(`"([A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+)"`)
	seen := map[string]bool{}
	for _, match := range namePattern.FindAllStringSubmatch(text, -1) {
		name := match[1]
		// SQLSTATE codes, header names and the like are not configuration.
		if !isConfigName(name) {
			continue
		}
		seen[name] = true
	}
	if len(seen) < 20 {
		t.Fatalf("only found %d configuration names; the pattern is probably wrong", len(seen))
	}

	for name := range seen {
		if !strings.Contains(loadBody, `"`+name+`"`) {
			t.Errorf("%s is named in this package but never read by Load; "+
				"the field it belongs to will hold its zero value in every deployment", name)
		}
	}
}

// isConfigName filters the regex hits down to plausible environment variables.
func isConfigName(name string) bool {
	for _, prefix := range []string{
		"APP_", "HTTP_", "DB_", "AUTH_", "RECEIPT_", "LOG_", "OBS_",
	} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func TestAuthDefaultsAreSafe(t *testing.T) {
	baseEnv(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("loading: %v", err)
	}

	// Lockout on by default. Off, the only brake on guessing one cashier's
	// password is the per-IP limiter, which at a university is keyed to a
	// campus NAT shared by a hall of terminals.
	if cfg.Auth.MaxLoginFailures <= 0 {
		t.Error("per-account lockout must be on by default")
	}
	if cfg.Auth.LoginFailureWindow <= 0 || cfg.Auth.LockoutDuration <= 0 {
		t.Error("the lockout window and duration must both be positive")
	}
	// Strict session checking on by default: logout, disablement and a role
	// change should take effect on the next request, not at the end of the
	// access token's life.
	if !cfg.Auth.StrictSessionCheck {
		t.Error("session revocation must be checked by default")
	}
}

func TestAuthOverridesAreHonoured(t *testing.T) {
	baseEnv(t)
	t.Setenv("AUTH_MAX_LOGIN_FAILURES", "3")
	t.Setenv("AUTH_LOGIN_FAILURE_WINDOW", "5m")
	t.Setenv("AUTH_LOCKOUT_DURATION", "30m")
	t.Setenv("AUTH_STRICT_SESSION_CHECK", "false")
	t.Setenv("AUTH_JWT_RETIRED_SECRETS", "old-one,old-two")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("loading: %v", err)
	}

	if cfg.Auth.MaxLoginFailures != 3 {
		t.Errorf("MaxLoginFailures = %d, want 3", cfg.Auth.MaxLoginFailures)
	}
	if cfg.Auth.LoginFailureWindow != 5*time.Minute {
		t.Errorf("LoginFailureWindow = %v, want 5m", cfg.Auth.LoginFailureWindow)
	}
	if cfg.Auth.LockoutDuration != 30*time.Minute {
		t.Errorf("LockoutDuration = %v, want 30m", cfg.Auth.LockoutDuration)
	}
	if cfg.Auth.StrictSessionCheck {
		t.Error("StrictSessionCheck should be off when set to false")
	}
	if len(cfg.Auth.RetiredJWTSecrets) != 2 {
		t.Errorf("RetiredJWTSecrets = %v, want two entries", cfg.Auth.RetiredJWTSecrets)
	}
}

// Production refuses a build it cannot identify, so an incident can always be
// traced to a tree and rolled back to a known-good predecessor.
func TestProductionRefusesAnUnidentifiedBuild(t *testing.T) {
	baseEnv(t)
	t.Setenv("APP_ENV", "production")
	t.Setenv("APP_VERSION", "unknown")
	t.Setenv("AUTH_JWT_SECRET", strings.Repeat("k", 48))
	t.Setenv("DB_SSLMODE", "require")
	t.Setenv("HTTP_CORS_ORIGINS", "https://fees.example.edu")

	_, err := config.Load()
	if err == nil {
		t.Fatal("production must refuse a build that cannot say what it is")
	}
	if !strings.Contains(err.Error(), "identifies nothing") {
		t.Errorf("the error should name the problem, got: %v", err)
	}

	// With a real version it loads.
	t.Setenv("APP_VERSION", "1.4.2")
	if _, err := config.Load(); err != nil {
		t.Fatalf("a properly stamped production build must load: %v", err)
	}
}

func TestProductionRefusesTheDevelopmentSecret(t *testing.T) {
	baseEnv(t)
	t.Setenv("APP_ENV", "production")
	t.Setenv("APP_VERSION", "1.4.2")
	t.Setenv("DB_SSLMODE", "require")
	t.Setenv("HTTP_CORS_ORIGINS", "https://fees.example.edu")

	_, err := config.Load()
	if err == nil {
		t.Fatal("production must refuse the development signing secret")
	}
}

// TestASecretCanComeFromAFile covers the deployment path that matters for the
// two settings worth protecting: a mounted secret rather than an environment
// variable that shows up in `docker inspect` and in /proc/<pid>/environ.
func TestASecretCanComeFromAFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jwt-secret")

	// The trailing newline is deliberate: every editor and every `echo` adds
	// one, and a secret that differs by a newline fails in a way that looks
	// exactly like the wrong secret.
	secret := "a-long-enough-secret-for-the-validator-to-accept-it"
	if err := os.WriteFile(path, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("AUTH_JWT_SECRET_FILE", path)
	t.Setenv("AUTH_JWT_SECRET", "the-environment-copy-that-should-lose")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if cfg.Auth.JWTSecret != secret {
		t.Errorf("the secret came from %q, want the file's contents", cfg.Auth.JWTSecret)
	}
}

// A file that cannot be read must not silently become the default. The
// environment is the fallback, and if that is absent too the validation that
// knows what the setting is for gets to refuse.
func TestAnUnreadableSecretFileFallsBackToTheEnvironment(t *testing.T) {
	t.Setenv("AUTH_JWT_SECRET_FILE", filepath.Join(t.TempDir(), "does-not-exist"))
	t.Setenv("AUTH_JWT_SECRET", "the-environment-copy-that-should-be-used-here")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if cfg.Auth.JWTSecret != "the-environment-copy-that-should-be-used-here" {
		t.Errorf("secret = %q, want the environment value", cfg.Auth.JWTSecret)
	}
}

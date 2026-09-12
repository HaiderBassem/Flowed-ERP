package config

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// A generated password is not URL-safe, and the failure it caused was remote
// from its cause: "/" ends the URL authority, so the connection string parsed
// as a host of "flowed" and a port made of password characters, and the process
// died reporting an invalid port that appeared nowhere in its configuration.
// `openssl rand -base64 32` produces a "/" about half the time, which made a
// first deployment a coin flip.
func TestDSNEscapesGeneratedPasswords(t *testing.T) {
	for _, password := range []string{
		"ab/cd",                                // ends the authority early
		"ab+cd=",                               // ordinary base64 padding
		"p@ssword",                             // splits the userinfo
		"colon:inside",                         // reads as a port
		"a?b#c",                                // starts a query, then a fragment
		"VVpviansJEGTHcpotd0S8Z/2vT1ZnVLFUMg=", // a real 32-byte base64 value
	} {
		db := Database{
			User:           "flowed",
			Password:       password,
			Host:           "postgres",
			Port:           5432,
			Name:           "flowed",
			SSLMode:        "require",
			ConnectTimeout: 10 * time.Second,
		}

		parsed, err := url.Parse(db.DSN())
		if err != nil {
			t.Fatalf("password %q produced an unparseable DSN: %v", password, err)
		}
		if got := parsed.Hostname(); got != "postgres" {
			t.Errorf("password %q: host = %q, want postgres", password, got)
		}
		if got := parsed.Port(); got != "5432" {
			t.Errorf("password %q: port = %q, want 5432", password, got)
		}
		if got := parsed.Path; got != "/flowed" {
			t.Errorf("password %q: database = %q, want /flowed", password, got)
		}
		if got := parsed.User.Username(); got != "flowed" {
			t.Errorf("password %q: user = %q, want flowed", password, got)
		}
		got, _ := parsed.User.Password()
		if got != password {
			t.Errorf("password did not survive the round trip: got %q, want %q", got, password)
		}
		if q := parsed.Query(); q.Get("sslmode") != "require" || q.Get("connect_timeout") != "10" {
			t.Errorf("password %q: lost query parameters: %q", password, parsed.RawQuery)
		}
	}
}

// The redacted form is what reaches the logs, and a password that leaks there
// is a password in a file every operator can read.
func TestRedactedDSNHidesThePassword(t *testing.T) {
	db := Database{
		User:     "flowed",
		Password: "VVpviansJEGTHcpotd0S8Z/2vT1ZnVLFUMg=",
		Host:     "postgres",
		Port:     5432,
		Name:     "flowed",
		SSLMode:  "require",
	}
	if strings.Contains(db.RedactedDSN(), db.Password) {
		t.Fatalf("the redacted DSN carries the password: %s", db.RedactedDSN())
	}
}

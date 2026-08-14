package shared

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ID is the surrogate identifier for every entity in the system. UUIDv7 is
// used throughout: it is globally unique like a UUIDv4 but its leading bits
// are a millisecond timestamp, so rows insert in roughly chronological order
// and PostgreSQL B-tree indexes stay dense instead of fragmenting the way
// random v4 keys do at millions of payments per year.
type ID = uuid.UUID

// NilID is the zero identifier.
var NilID = uuid.Nil

// NewID mints a time-ordered identifier.
func NewID() ID {
	id, err := uuid.NewV7()
	if err != nil {
		// NewV7 only fails if the system entropy source is broken, in which
		// case a random v4 is still a correct unique identifier.
		return uuid.New()
	}
	return id
}

// ParseID reads an identifier from its canonical text form.
func ParseID(s string) (ID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return NilID, Validation("invalid_id", "%q is not a valid identifier", s).WithCause(err)
	}
	return id, nil
}

// IsNil reports whether the identifier is unset.
func IsNil(id ID) bool { return id == uuid.Nil }

// Clock abstracts the current time so that command handlers stay testable and
// so that a single request stamps every row it writes with one consistent
// instant. Business code never calls time.Now directly.
type Clock interface {
	Now() time.Time
}

// SystemClock reads the wall clock in UTC. Every timestamp the system persists
// is UTC; Baghdad local time is applied only when rendering.
type SystemClock struct{}

// Now returns the current UTC time.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// FixedClock returns a constant instant, for tests and for replaying a batch
// under a single logical timestamp.
type FixedClock struct{ Instant time.Time }

// Now returns the fixed instant.
func (c FixedClock) Now() time.Time { return c.Instant }

// Date is a calendar date with no time or zone component: an installment due
// date, a birth date, a court decision date. Storing these as timestamps is a
// recurring source of off-by-one-day bugs across time zones, so they carry
// their own type mapping to a PostgreSQL DATE column.
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

// NewDate builds a calendar date.
func NewDate(year int, month time.Month, day int) Date {
	return Date{Year: year, Month: month, Day: day}
}

// DateFromTime projects an instant onto a calendar date in UTC.
func DateFromTime(t time.Time) Date {
	utc := t.UTC()
	return Date{Year: utc.Year(), Month: utc.Month(), Day: utc.Day()}
}

// ParseDate reads an ISO-8601 calendar date, "2025-09-30".
func ParseDate(s string) (Date, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return Date{}, Validation("invalid_date", "%q is not a valid date (expected YYYY-MM-DD)", s).WithCause(err)
	}
	return DateFromTime(t), nil
}

// Time converts the date to midnight UTC.
func (d Date) Time() time.Time {
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC)
}

// String renders the date in ISO-8601.
func (d Date) String() string {
	return fmt.Sprintf("%04d-%02d-%02d", d.Year, int(d.Month), d.Day)
}

// IsZero reports whether the date is unset.
func (d Date) IsZero() bool { return d.Year == 0 && d.Month == 0 && d.Day == 0 }

// Before reports whether d falls strictly before other.
func (d Date) Before(other Date) bool { return d.Time().Before(other.Time()) }

// After reports whether d falls strictly after other.
func (d Date) After(other Date) bool { return d.Time().After(other.Time()) }

// AddDays returns the date shifted by n days.
func (d Date) AddDays(n int) Date { return DateFromTime(d.Time().AddDate(0, 0, n)) }

// MarshalJSON encodes the date as an ISO-8601 string.
func (d Date) MarshalJSON() ([]byte, error) {
	return []byte(`"` + d.String() + `"`), nil
}

// UnmarshalJSON decodes an ISO-8601 date string.
func (d *Date) UnmarshalJSON(data []byte) error {
	s := string(data)
	if s == "null" {
		*d = Date{}
		return nil
	}
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return Validation("invalid_date", "date must be a JSON string")
	}
	parsed, err := ParseDate(s[1 : len(s)-1])
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

// Package notify holds the rules for telling a student that money is due.
//
// The aging report tells the finance office who is late. Nothing told the
// student, which means the first they hear of a missed installment is a
// registration block in September — and a collection rate is a direct
// consequence of that silence.
//
// Two rules shape everything here. A reminder is decided from the schedule
// rather than from a timer, so a job that runs twice because a deploy restarted
// it produces one message; and the message is written for somebody standing at
// a counter with it in their hand, which is why the amount and the date are in
// it rather than a link.
package notify

import (
	"fmt"
	"sort"
	"strings"

	"flowed/internal/domain/money"
	"flowed/internal/domain/shared"
)

// Kind is what a message is about.
type Kind string

const (
	// KindUpcoming warns before an installment falls due.
	KindUpcoming Kind = "upcoming_due"
	// KindOverdue chases one that has.
	KindOverdue Kind = "overdue"
	// KindReceipt confirms a collection.
	KindReceipt Kind = "receipt_issued"
	// KindClearanceBlocked tells a student their graduation is held.
	KindClearanceBlocked Kind = "clearance_blocked"
	// KindSponsorInvoice tells a sponsor what they owe.
	KindSponsorInvoice Kind = "sponsor_invoice"
)

// Channel is how a message travels.
type Channel string

const (
	// ChannelSMS is what reaches an Iraqi student. Most have no e-mail they
	// read and every one has a phone.
	ChannelSMS Channel = "sms"
	// ChannelEmail is for sponsors and for the few students who use one.
	ChannelEmail Channel = "email"
	// ChannelNone queues nothing and leaves a worklist. A university with no
	// gateway still gets the list of who to telephone, which is what it was
	// doing anyway.
	ChannelNone Channel = "none"
)

// Status is what became of a message.
type Status string

const (
	StatusPending   Status = "pending"
	StatusSent      Status = "sent"
	StatusFailed    Status = "failed"
	StatusSkipped   Status = "skipped"
	StatusCancelled Status = "cancelled"
)

// Schedule is a year's reminder policy.
//
// Per year, beside the debt-block and clearance policies, because a university
// changes its mind about how hard to chase and every past year must keep
// showing the rule that applied to it.
type Schedule struct {
	Enabled    bool
	DaysBefore []int
	DaysAfter  []int
	Channel    Channel
}

// DueReminder is one message the schedule says should exist.
type DueReminder struct {
	Kind Kind
	// Offset is days before (negative) or after (positive) the due date.
	Offset int
	// WindowKey makes a duplicate detectable: one message per student, kind
	// and window, per installment. The seven-day warning and the one-day
	// warning are two windows; a second run of either is neither.
	WindowKey string
}

// Due decides which reminders an installment warrants today.
//
// Returns nothing when the schedule is off, when the installment is settled,
// or when today is not one of the days the policy names. Deliberately a pure
// function of (schedule, due date, today): a job that runs at nine and again at
// noon computes the same answer, and the duplicate guard in the database turns
// the second into a no-op.
func Due(schedule Schedule, dueDate, today shared.Date, remaining money.Amount) []DueReminder {
	if !schedule.Enabled || !remaining.IsPositive() {
		return nil
	}

	// Positive once the due date has passed.
	elapsed := today.DaysSince(dueDate)

	var reminders []DueReminder
	if elapsed < 0 {
		daysUntil := -elapsed
		for _, offset := range schedule.DaysBefore {
			if offset == daysUntil {
				reminders = append(reminders, DueReminder{
					Kind:      KindUpcoming,
					Offset:    -offset,
					WindowKey: fmt.Sprintf("%s:before:%d", dueDate.String(), offset),
				})
			}
		}
		return reminders
	}

	for _, offset := range schedule.DaysAfter {
		if offset == elapsed {
			reminders = append(reminders, DueReminder{
				Kind:      KindOverdue,
				Offset:    offset,
				WindowKey: fmt.Sprintf("%s:after:%d", dueDate.String(), offset),
			})
		}
	}
	return reminders
}

// Render fills a template's placeholders.
//
// Placeholders rather than a template language: the bodies are edited by a
// finance officer, not a programmer, and an expression language in a field
// somebody edits at a desk is a way to produce a message nobody can predict.
// An unknown placeholder is left as it stands, so a typo shows up in the
// message where somebody will notice rather than silently emptying it.
func Render(body string, values map[string]string) string {
	if body == "" {
		return ""
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	// Longest first, so {{outstanding}} is not eaten by a shorter {{out}}.
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })

	rendered := body
	for _, key := range keys {
		rendered = strings.ReplaceAll(rendered, "{{"+key+"}}", values[key])
	}
	return strings.TrimSpace(rendered)
}

// Destination picks where a message should go.
//
// A student's own number first, their guardian's second. In Iraq a first-year
// student's telephone is frequently their father's, and a reminder that reaches
// nobody because one field was blank is a reminder that did not happen.
func Destination(channel Channel, studentPhone, guardianPhone, email *string) (string, error) {
	switch channel {
	case ChannelSMS:
		for _, candidate := range []*string{studentPhone, guardianPhone} {
			if candidate != nil && strings.TrimSpace(*candidate) != "" {
				return strings.TrimSpace(*candidate), nil
			}
		}
		return "", shared.Validation("notify.no_phone",
			"neither the student nor their guardian has a telephone number on file")
	case ChannelEmail:
		if email != nil && strings.TrimSpace(*email) != "" {
			return strings.TrimSpace(*email), nil
		}
		return "", shared.Validation("notify.no_email", "this student has no e-mail address on file")
	case ChannelNone:
		return "", nil
	default:
		return "", shared.Validation("notify.unknown_channel", "%q is not a channel", channel)
	}
}

package notify_test

import (
	"testing"
	"time"

	"github.com/swibit/flowed/internal/domain/notify"
	"github.com/swibit/flowed/internal/domain/shared"
)

func schedule() notify.Schedule {
	return notify.Schedule{
		Enabled:    true,
		DaysBefore: []int{7, 1},
		DaysAfter:  []int{1, 7, 30},
		Channel:    notify.ChannelSMS,
	}
}

func date(y, m, d int) shared.Date { return shared.NewDate(y, time.Month(m), d) }

func TestRemindersFireOnlyOnTheDaysThePolicyNames(t *testing.T) {
	due := date(2026, 3, 15)

	cases := map[string]struct {
		today shared.Date
		want  int
		kind  notify.Kind
	}{
		"seven days before": {date(2026, 3, 8), 1, notify.KindUpcoming},
		"one day before":    {date(2026, 3, 14), 1, notify.KindUpcoming},
		"three days before": {date(2026, 3, 12), 0, ""},
		"on the day":        {date(2026, 3, 15), 0, ""},
		"one day after":     {date(2026, 3, 16), 1, notify.KindOverdue},
		"seven days after":  {date(2026, 3, 22), 1, notify.KindOverdue},
		"ten days after":    {date(2026, 3, 25), 0, ""},
		"thirty days after": {date(2026, 4, 14), 1, notify.KindOverdue},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			reminders := notify.Due(schedule(), due, tc.today, 500_000)
			if len(reminders) != tc.want {
				t.Fatalf("got %d reminder(s), want %d", len(reminders), tc.want)
			}
			if tc.want > 0 && reminders[0].Kind != tc.kind {
				t.Errorf("kind = %q, want %q", reminders[0].Kind, tc.kind)
			}
		})
	}
}

// The property that makes a scheduler restart harmless: the same inputs
// produce the same window key, and the database refuses the second row.
func TestWindowKeyIsStableAndDistinguishesOffsets(t *testing.T) {
	due := date(2026, 3, 15)

	first := notify.Due(schedule(), due, date(2026, 3, 8), 500_000)
	again := notify.Due(schedule(), due, date(2026, 3, 8), 500_000)
	if len(first) != 1 || len(again) != 1 {
		t.Fatal("expected one reminder from each call")
	}
	if first[0].WindowKey != again[0].WindowKey {
		t.Errorf("the same day produced two window keys: %q and %q",
			first[0].WindowKey, again[0].WindowKey)
	}

	// The seven-day warning and the one-day warning are different messages.
	oneDay := notify.Due(schedule(), due, date(2026, 3, 14), 500_000)
	if oneDay[0].WindowKey == first[0].WindowKey {
		t.Error("two different warnings must not share a window key")
	}
}

func TestNothingIsSentForASettledInstallment(t *testing.T) {
	if reminders := notify.Due(schedule(), date(2026, 3, 15), date(2026, 3, 16), 0); len(reminders) != 0 {
		t.Fatalf("a paid installment produced %d reminder(s)", len(reminders))
	}
}

func TestNothingIsSentWhenTheYearHasRemindersOff(t *testing.T) {
	off := schedule()
	off.Enabled = false
	if reminders := notify.Due(off, date(2026, 3, 15), date(2026, 3, 16), 500_000); len(reminders) != 0 {
		t.Fatalf("a disabled schedule produced %d reminder(s)", len(reminders))
	}
}

func TestRenderFillsPlaceholders(t *testing.T) {
	body := "عزيزي {{student_name}}، يستحق مبلغ {{amount}} بتاريخ {{due_date}}. المتبقي {{outstanding}}."
	rendered := notify.Render(body, map[string]string{
		"student_name": "علي محمد",
		"amount":       "500,000",
		"due_date":     "2026-03-15",
		"outstanding":  "1,500,000",
	})

	for _, want := range []string{"علي محمد", "500,000", "2026-03-15", "1,500,000"} {
		if !contains(rendered, want) {
			t.Errorf("rendered message is missing %q: %s", want, rendered)
		}
	}
	if contains(rendered, "{{") {
		t.Errorf("a placeholder survived rendering: %s", rendered)
	}
}

// An unknown placeholder is left where somebody will see it, rather than
// silently emptying the message.
func TestUnknownPlaceholderSurvivesVisibly(t *testing.T) {
	rendered := notify.Render("due {{amount}} on {{dua_date}}", map[string]string{"amount": "100"})
	if !contains(rendered, "{{dua_date}}") {
		t.Errorf("the typo should be visible in the message, got %q", rendered)
	}
}

// In Iraq a first-year student's telephone is frequently their father's, and a
// reminder that reaches nobody because one field was blank did not happen.
func TestDestinationFallsBackToTheGuardian(t *testing.T) {
	guardian := "07701234567"

	got, err := notify.Destination(notify.ChannelSMS, nil, &guardian, nil)
	if err != nil {
		t.Fatalf("falling back to the guardian: %v", err)
	}
	if got != guardian {
		t.Errorf("destination = %q, want the guardian's number", got)
	}

	own := "07809876543"
	got, err = notify.Destination(notify.ChannelSMS, &own, &guardian, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != own {
		t.Errorf("the student's own number should win, got %q", got)
	}

	if _, err := notify.Destination(notify.ChannelSMS, nil, nil, nil); err == nil {
		t.Error("with no number at all the reminder must be refused rather than queued")
	}
}

func TestNoneChannelQueuesNothing(t *testing.T) {
	got, err := notify.Destination(notify.ChannelNone, nil, nil, nil)
	if err != nil || got != "" {
		t.Errorf("the none channel should produce an empty destination, got %q, %v", got, err)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		(haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

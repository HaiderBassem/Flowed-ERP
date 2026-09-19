package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"flowed/internal/domain/money"
	"flowed/internal/domain/notify"
	"flowed/internal/domain/shared"
	"flowed/internal/port"
)

// NotifyService queues due-date reminders and hands them to whatever channel
// the deployment has.
//
// Deliberately outside the financial core. It reads what is owed and writes
// nothing to any account: a reminder that could change a balance would be a
// reminder somebody would eventually use to change one. What it produces is a
// row per message and, when a gateway exists, a delivery — and when none
// exists, a worklist a clerk can telephone from, which is what the office was
// doing anyway.
type NotifyService struct {
	deps      Deps
	repo      port.NotificationRepository
	deliverer port.Deliverer
	// institution appears in every message so a student knows who is asking
	// them for money.
	institution string
	auditor
}

// NewNotifyService wires the reminder commands.
func NewNotifyService(d Deps, repo port.NotificationRepository, deliverer port.Deliverer, institution string) *NotifyService {
	return &NotifyService{
		deps: d, repo: repo, deliverer: deliverer, institution: institution,
		auditor: newAuditor(d.Audit, d.Clock),
	}
}

// QueueDueRemindersResult reports what a scheduling pass produced.
type QueueDueRemindersResult struct {
	Considered int
	Queued     int
	// Skipped counts obligations whose day was not one the policy names, plus
	// the messages that already existed. Both are the normal outcome.
	Skipped int
	// Undeliverable counts students with no telephone number at all. Worth
	// seeing: it is a data-quality problem the office can fix.
	Undeliverable int
}

// QueueDueReminders writes the messages today's schedule calls for.
//
// The window is bounded by the widest offset any year configures, so the query
// reads the installments that could possibly matter rather than all of them.
// Everything after that is the pure decision in the domain, and the database's
// unique window index makes a second run a no-op.
func (s *NotifyService) QueueDueReminders(ctx context.Context, limit int) (*QueueDueRemindersResult, error) {
	today := shared.DateFromTime(nowOr(s.deps.Clock))
	channel := notify.ChannelNone
	if s.deliverer != nil {
		channel = s.deliverer.Channel()
	}

	// Widest window any year could ask for. Generous rather than computed per
	// year: the query is bounded either way, and a year configured with a
	// sixty-day chase should not be silently ignored.
	const widestWindow = 90
	from := today.AddDays(-widestWindow)
	to := today.AddDays(widestWindow)

	due, err := s.repo.DueInstallments(ctx, from, to, limit)
	if err != nil {
		return nil, err
	}

	result := &QueueDueRemindersResult{Considered: len(due)}
	for _, item := range due {
		schedule := notify.Schedule{
			Enabled:    item.RemindersEnabled,
			DaysBefore: toInts(item.DaysBefore),
			DaysAfter:  toInts(item.DaysAfter),
			Channel:    channel,
		}
		reminders := notify.Due(schedule, item.DueDate, today, money.Amount(item.Remaining))
		if len(reminders) == 0 {
			result.Skipped++
			continue
		}

		destination, destErr := notify.Destination(channel, item.Phone, item.GuardianPhone, item.Email)
		if destErr != nil {
			// Recorded as undeliverable rather than dropped: a student with no
			// number on file is a data problem the office can fix, and it is
			// only visible if somebody counts it.
			result.Undeliverable++
			continue
		}

		for _, reminder := range reminders {
			queued, err := s.queueOne(ctx, item, reminder, channel, destination)
			if err != nil {
				s.logf(ctx, "queueing a reminder", err)
				continue
			}
			if queued {
				result.Queued++
			} else {
				result.Skipped++
			}
		}
	}
	return result, nil
}

func (s *NotifyService) queueOne(
	ctx context.Context, item port.DueInstallment, reminder notify.DueReminder,
	channel notify.Channel, destination string,
) (bool, error) {
	templateCode := "UPCOMING_DUE"
	if reminder.Kind == notify.KindOverdue {
		templateCode = "OVERDUE"
	}

	body := ""
	template, err := s.repo.Template(ctx, templateCode, channel)
	if err == nil && template != nil {
		body = notify.Render(template.BodyAr, map[string]string{
			"student_name": item.StudentName,
			"student_no":   item.StudentNo,
			"amount":       money.Amount(item.Remaining).String(),
			"due_date":     item.DueDate.String(),
			"outstanding":  money.Amount(item.Outstanding).String(),
			"year":         item.YearCode,
			"university":   s.institution,
		})
	}
	if strings.TrimSpace(body) == "" {
		// A missing or empty template must not stop the reminder: the message
		// matters more than its wording, and a fallback is visible enough that
		// somebody will fix the template.
		body = fmt.Sprintf("%s: %s IQD due %s (outstanding %s IQD)",
			item.StudentNo, money.Amount(item.Remaining), item.DueDate,
			money.Amount(item.Outstanding))
	}

	notification := &port.Notification{
		ID:             shared.NewID(),
		Kind:           reminder.Kind,
		Channel:        channel,
		StudentID:      item.StudentID,
		AccountID:      &item.AccountID,
		InstallmentID:  &item.InstallmentID,
		AcademicYearID: &item.AcademicYearID,
		Body:           body,
		Status:         notify.StatusPending,
		WindowKey:      reminder.WindowKey,
		ScheduledFor:   nowOr(s.deps.Clock),
	}
	if destination != "" {
		notification.Destination = &destination
	}
	if channel == notify.ChannelNone {
		// Nothing will deliver it; it is a worklist entry, and marking it so
		// keeps the delivery queue honest about what is actually waiting.
		notification.Status = notify.StatusSkipped
	}

	var queued bool
	err = s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		var err error
		queued, err = s.repo.Queue(ctx, notification)
		return err
	})
	return queued, err
}

// DeliverPendingResult reports what a delivery pass did.
type DeliverPendingResult struct {
	Attempted int
	Sent      int
	Failed    int
}

// DeliverPending hands queued messages to the channel.
//
// A failure is recorded against the message and retried on the next pass, up to
// a bounded number of attempts. A gateway that was down for an hour should not
// lose a day's reminders; one that has been down for a week should stop being
// retried and start being noticed.
func (s *NotifyService) DeliverPending(ctx context.Context, limit int) (*DeliverPendingResult, error) {
	if s.deliverer == nil {
		return &DeliverPendingResult{}, nil
	}

	pending, err := s.repo.Pending(ctx, limit)
	if err != nil {
		return nil, err
	}

	result := &DeliverPendingResult{Attempted: len(pending)}
	for _, message := range pending {
		if message.Destination == nil || *message.Destination == "" {
			if err := s.repo.MarkFailed(ctx, message.ID, "no destination on file", nowOr(s.deps.Clock)); err != nil {
				s.logf(ctx, "recording an undeliverable reminder", err)
			}
			result.Failed++
			continue
		}

		if err := s.deliverer.Send(ctx, *message.Destination, message.Body); err != nil {
			if markErr := s.repo.MarkFailed(ctx, message.ID, err.Error(), nowOr(s.deps.Clock)); markErr != nil {
				s.logf(ctx, "recording a failed delivery", markErr)
			}
			result.Failed++
			continue
		}
		if err := s.repo.MarkSent(ctx, message.ID, nowOr(s.deps.Clock)); err != nil {
			s.logf(ctx, "recording a delivery", err)
		}
		result.Sent++
	}
	return result, nil
}

// StudentNotifications returns what a student has been told.
func (s *NotifyService) StudentNotifications(ctx context.Context, actor shared.Actor, studentID shared.ID, limit int) ([]*port.Notification, error) {
	if actor.HasRole(shared.RoleStudent) {
		if actor.StudentID == nil || *actor.StudentID != studentID {
			return nil, shared.Forbidden("notify.not_yours", "this record belongs to another student")
		}
	} else if err := actor.RequireAnyRole("StudentNotifications",
		shared.RoleRegistrar, shared.RoleFinanceManager, shared.RoleAdmin,
		shared.RoleCashier, shared.RoleAuditor); err != nil {
		return nil, err
	}
	return s.repo.ListForStudent(ctx, studentID, limit)
}

// Templates returns the wording of every message.
func (s *NotifyService) Templates(ctx context.Context, actor shared.Actor) ([]*port.NotificationTemplate, error) {
	if err := actor.RequireAnyRole("NotificationTemplates",
		shared.RoleFinanceManager, shared.RoleAdmin); err != nil {
		return nil, err
	}
	return s.repo.ListTemplates(ctx)
}

// UpdateTemplate rewords a message.
func (s *NotifyService) UpdateTemplate(ctx context.Context, actor shared.Actor, id shared.ID, bodyAr string, bodyEn *string, active bool) error {
	if err := actor.RequireAnyRole("UpdateNotificationTemplate",
		shared.RoleFinanceManager, shared.RoleAdmin); err != nil {
		return err
	}
	if strings.TrimSpace(bodyAr) == "" {
		return shared.Validation("notify.body_required", "a message needs an Arabic body")
	}

	return s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		template := &port.NotificationTemplate{ID: id, BodyAr: bodyAr, BodyEn: bodyEn, IsActive: active}
		if err := s.repo.UpdateTemplate(ctx, template); err != nil {
			return err
		}
		return s.record(ctx, port.AuditEntry{
			EntityType: "notification_template",
			EntityID:   &id,
			Action:     "notification.template_updated",
			Actor:      actor,
		})
	})
}

func (s *NotifyService) logf(ctx context.Context, what string, err error) {
	if s.deps.Log == nil {
		return
	}
	s.deps.Log.WarnContext(ctx, what, slog.String("error", err.Error()))
}

func toInts(values []int16) []int {
	out := make([]int, 0, len(values))
	for _, v := range values {
		out = append(out, int(v))
	}
	return out
}

var _ = time.Now

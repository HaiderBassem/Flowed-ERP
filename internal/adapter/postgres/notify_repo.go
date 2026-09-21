package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"flowed/internal/domain/notify"
	"flowed/internal/domain/shared"
	"flowed/internal/platform/pg"
	"flowed/internal/port"
)

// NotificationRepository stores messages and reads the schedule that produces
// them.
type NotificationRepository struct{ db *pg.DB }

// NewNotificationRepository builds the notification store over a pool.
func NewNotificationRepository(db *pg.DB) *NotificationRepository {
	return &NotificationRepository{db: db}
}

var _ port.NotificationRepository = (*NotificationRepository)(nil)

const notificationColumns = `
	id, kind, channel, student_id, account_id, installment_id, academic_year_id,
	destination, body, status, attempts, last_error, window_key,
	scheduled_for, created_at, sent_at, failed_at`

func scanNotification(row pgx.Row) (*port.Notification, error) {
	var n port.Notification
	if err := row.Scan(
		&n.ID, &n.Kind, &n.Channel, &n.StudentID, &n.AccountID, &n.InstallmentID,
		&n.AcademicYearID, &n.Destination, &n.Body, &n.Status, &n.Attempts, &n.LastError,
		&n.WindowKey, &n.ScheduledFor, &n.CreatedAt, &n.SentAt, &n.FailedAt,
	); err != nil {
		return nil, err
	}
	return &n, nil
}

// Queue records a message, refusing a duplicate.
//
// ON CONFLICT DO NOTHING against the unique window index is the guard itself,
// and it is one statement so two schedulers racing cannot both conclude they
// are the first. A student who receives three copies of a dunning message stops
// reading them, which costs more than the message was worth.
func (r *NotificationRepository) Queue(ctx context.Context, n *port.Notification) (bool, error) {
	if err := r.db.RequireTx(ctx, "notification.Queue"); err != nil {
		return false, err
	}
	const query = `
		INSERT INTO notification (id, kind, channel, student_id, account_id, installment_id,
			academic_year_id, destination, body, status, window_key, scheduled_for)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,COALESCE($12, now()))
		ON CONFLICT DO NOTHING
		RETURNING created_at`

	if n.ID == shared.NilID {
		n.ID = shared.NewID()
	}
	if n.Status == "" {
		n.Status = notify.StatusPending
	}

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query, n.ID, string(n.Kind), string(n.Channel), n.StudentID,
		n.AccountID, n.InstallmentID, n.AcademicYearID, n.Destination, n.Body,
		string(n.Status), n.WindowKey, instant(n.ScheduledFor)).Scan(&n.CreatedAt)
	if err != nil {
		if pg.IsNotFound(err) {
			return false, nil
		}
		return false, pg.WrapQuery("notification.Queue", err)
	}
	return true, nil
}

// MarkSent records a delivery.
func (r *NotificationRepository) MarkSent(ctx context.Context, id shared.ID, at time.Time) error {
	const query = `
		UPDATE notification
		SET status = 'sent', sent_at = COALESCE($2, now()), attempts = attempts + 1
		WHERE id = $1`

	q := r.db.Conn(ctx)
	_, err := q.Exec(ctx, query, id, instant(at))
	return pg.WrapQuery("notification.MarkSent", err)
}

// MarkFailed records an attempt that did not deliver.
//
// The message stays pending until it has been tried several times: a gateway
// that was down for an hour should not lose a day's reminders. After that it
// fails visibly rather than being retried forever.
func (r *NotificationRepository) MarkFailed(ctx context.Context, id shared.ID, reason string, at time.Time) error {
	const query = `
		UPDATE notification SET
			attempts = attempts + 1,
			last_error = $2,
			status = CASE WHEN attempts + 1 >= 5 THEN 'failed' ELSE 'pending' END,
			failed_at = CASE WHEN attempts + 1 >= 5 THEN COALESCE($3, now()) ELSE NULL END
		WHERE id = $1`

	q := r.db.Conn(ctx)
	_, err := q.Exec(ctx, query, id, reason, instant(at))
	return pg.WrapQuery("notification.MarkFailed", err)
}

// Pending returns messages waiting to be delivered.
func (r *NotificationRepository) Pending(ctx context.Context, limit int) ([]*port.Notification, error) {
	const query = `
		SELECT` + notificationColumns + `
		FROM notification
		WHERE status = 'pending' AND scheduled_for <= now()
		ORDER BY scheduled_for
		LIMIT $1`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, boundedLimit(limit, 100))
	if err != nil {
		return nil, pg.WrapQuery("notification.Pending", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*port.Notification, error) {
		return scanNotification(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("notification.Pending", err)
	}
	return out, nil
}

// ListForStudent returns what a student has been told.
func (r *NotificationRepository) ListForStudent(ctx context.Context, studentID shared.ID, limit int) ([]*port.Notification, error) {
	const query = `
		SELECT` + notificationColumns + `
		FROM notification WHERE student_id = $1 ORDER BY created_at DESC LIMIT $2`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, studentID, boundedLimit(limit, 50))
	if err != nil {
		return nil, pg.WrapQuery("notification.ListForStudent", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*port.Notification, error) {
		return scanNotification(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("notification.ListForStudent", err)
	}
	return out, nil
}

// DueInstallments returns obligations inside the reminder window, with the
// student's contact details and the year's policy alongside.
//
// One query rather than a query per student: a nightly job over fifty thousand
// students would otherwise be fifty thousand round trips, and the reason
// reminders do not exist in most systems is that somebody wrote them that way
// once and turned them off again.
func (r *NotificationRepository) DueInstallments(
	ctx context.Context, from, to shared.Date, limit int,
) ([]port.DueInstallment, error) {
	const query = `
		SELECT i.id, i.account_id, fa.student_id, fa.academic_year_id, ay.code,
		       i.due_date, i.amount, i.amount - i.paid_amount,
		       coalesce(b.outstanding, 0)::bigint,
		       s.student_no, s.full_name, s.phone, s.guardian_phone, s.email,
		       ay.reminders_enabled, ay.reminder_days_before, ay.reminder_days_after
		FROM installment i
		JOIN financial_account fa ON fa.id = i.account_id
		JOIN academic_year ay ON ay.id = fa.academic_year_id
		JOIN student s ON s.id = fa.student_id
		LEFT JOIN v_account_balance b ON b.account_id = fa.id
		WHERE i.status IN ('pending', 'partially_paid')
		  AND i.due_date BETWEEN $1 AND $2
		  AND fa.status IN ('active', 'pending')
		  AND ay.reminders_enabled
		  AND s.status <> 'merged'
		ORDER BY i.due_date
		LIMIT $3`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, from.Time(), to.Time(), boundedLimit(limit, 5000))
	if err != nil {
		return nil, pg.WrapQuery("notification.DueInstallments", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (port.DueInstallment, error) {
		var (
			d       port.DueInstallment
			dueDate time.Time
		)
		err := row.Scan(&d.InstallmentID, &d.AccountID, &d.StudentID, &d.AcademicYearID,
			&d.YearCode, &dueDate, &d.Amount, &d.Remaining, &d.Outstanding,
			&d.StudentNo, &d.StudentName, &d.Phone, &d.GuardianPhone, &d.Email,
			&d.RemindersEnabled, &d.DaysBefore, &d.DaysAfter)
		d.DueDate = shared.DateFromTime(dueDate)
		return d, err
	})
	if err != nil {
		return nil, pg.WrapQuery("notification.DueInstallments", err)
	}
	return out, nil
}

const notifyTemplateColumns = ` id, code, channel, body_ar, body_en, is_active`

// Template returns the wording of one kind of message.
func (r *NotificationRepository) Template(ctx context.Context, code string, channel notify.Channel) (*port.NotificationTemplate, error) {
	const query = `SELECT` + notifyTemplateColumns + `
		FROM notification_template WHERE code = $1 AND channel = $2 AND is_active`

	q := r.db.Conn(ctx)
	var t port.NotificationTemplate
	err := q.QueryRow(ctx, query, code, string(channel)).
		Scan(&t.ID, &t.Code, &t.Channel, &t.BodyAr, &t.BodyEn, &t.IsActive)
	if err != nil {
		return nil, pg.WrapQuery("notification.Template", err)
	}
	return &t, nil
}

// ListTemplates returns every wording, for the administration screen.
func (r *NotificationRepository) ListTemplates(ctx context.Context) ([]*port.NotificationTemplate, error) {
	const query = `SELECT` + notifyTemplateColumns + ` FROM notification_template ORDER BY code, channel`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query)
	if err != nil {
		return nil, pg.WrapQuery("notification.ListTemplates", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*port.NotificationTemplate, error) {
		var t port.NotificationTemplate
		err := row.Scan(&t.ID, &t.Code, &t.Channel, &t.BodyAr, &t.BodyEn, &t.IsActive)
		return &t, err
	})
	if err != nil {
		return nil, pg.WrapQuery("notification.ListTemplates", err)
	}
	return out, nil
}

// UpdateTemplate rewords a message.
func (r *NotificationRepository) UpdateTemplate(ctx context.Context, t *port.NotificationTemplate) error {
	if err := r.db.RequireTx(ctx, "notification.UpdateTemplate"); err != nil {
		return err
	}
	const query = `
		UPDATE notification_template SET body_ar = $2, body_en = $3, is_active = $4
		WHERE id = $1 RETURNING id`

	q := r.db.Conn(ctx)
	var id shared.ID
	err := q.QueryRow(ctx, query, t.ID, t.BodyAr, t.BodyEn, t.IsActive).Scan(&id)
	return pg.WrapQuery("notification.UpdateTemplate", err)
}

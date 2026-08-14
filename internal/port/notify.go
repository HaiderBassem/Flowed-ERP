package port

import (
	"context"
	"time"

	"github.com/swibit/flowed/internal/domain/notify"
	"github.com/swibit/flowed/internal/domain/shared"
)

// Notification is one message that should reach a student.
type Notification struct {
	ID             shared.ID
	Kind           notify.Kind
	Channel        notify.Channel
	StudentID      shared.ID
	AccountID      *shared.ID
	InstallmentID  *shared.ID
	AcademicYearID *shared.ID
	Destination    *string
	Body           string
	Status         notify.Status
	Attempts       int
	LastError      *string
	WindowKey      string
	ScheduledFor   time.Time
	CreatedAt      time.Time
	SentAt         *time.Time
	FailedAt       *time.Time
}

// NotificationTemplate is the wording of one kind of message.
type NotificationTemplate struct {
	ID       shared.ID
	Code     string
	Channel  notify.Channel
	BodyAr   string
	BodyEn   *string
	IsActive bool
}

// DueInstallment is one obligation the reminder job considers.
type DueInstallment struct {
	InstallmentID  shared.ID
	AccountID      shared.ID
	StudentID      shared.ID
	AcademicYearID shared.ID
	YearCode       string
	DueDate        shared.Date
	Amount         int64
	Remaining      int64
	Outstanding    int64
	StudentNo      string
	StudentName    string
	Phone          *string
	GuardianPhone  *string
	Email          *string
	// The year's policy, read with the row so the job does not query it per
	// student.
	RemindersEnabled bool
	DaysBefore       []int16
	DaysAfter        []int16
}

// NotificationRepository stores messages and the schedule that produces them.
type NotificationRepository interface {
	// Queue records a message. Returns false when one already exists for this
	// student, kind and window — which is what makes a scheduler that ran
	// twice send once.
	Queue(ctx context.Context, n *Notification) (queued bool, err error)
	MarkSent(ctx context.Context, id shared.ID, at time.Time) error
	MarkFailed(ctx context.Context, id shared.ID, reason string, at time.Time) error
	// Pending returns messages waiting to be delivered, oldest first.
	Pending(ctx context.Context, limit int) ([]*Notification, error)
	ListForStudent(ctx context.Context, studentID shared.ID, limit int) ([]*Notification, error)

	// DueInstallments returns the obligations whose due date falls inside the
	// window the schedule could possibly act on, with the student's contact
	// details and the year's policy alongside.
	DueInstallments(ctx context.Context, from, to shared.Date, limit int) ([]DueInstallment, error)
	Template(ctx context.Context, code string, channel notify.Channel) (*NotificationTemplate, error)
	ListTemplates(ctx context.Context) ([]*NotificationTemplate, error)
	UpdateTemplate(ctx context.Context, t *NotificationTemplate) error
}

// Deliverer hands a message to a channel.
//
// An interface so the financial core never knows whether the university has an
// SMS gateway, a contract with a bulk provider, or a clerk with a telephone.
// A deployment with none configured still gets the worklist, which is the part
// that has value on its own.
type Deliverer interface {
	// Channel is what this deliverer sends over.
	Channel() notify.Channel
	// Send delivers one message. An error means "not delivered"; the caller
	// records the attempt and retries later.
	Send(ctx context.Context, destination, body string) error
}

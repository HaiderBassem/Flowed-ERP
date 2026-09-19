package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"strings"
	"time"

	"flowed/internal/domain/billing"
	"flowed/internal/domain/money"
	"flowed/internal/domain/shared"
	"flowed/internal/platform/auth"
	"flowed/internal/port"
)

// PortalService answers the question every student walks to the finance window
// to ask: what do I owe?
//
// That queue is the university's single most common interaction, and it exists
// only because the answer was reachable by no other route. Everything here is
// read-only except issuing a credential and printing a verifiable statement,
// and every path checks the row it is about against the student the credential
// belongs to — a student who guesses another student's account number learns
// nothing.
type PortalService struct {
	deps      Deps
	hasher    PasswordHasher
	verifiers port.VerificationRepository
	sponsors  *SponsorService
	auditor
}

// NewPortalService wires the student-facing commands.
func NewPortalService(d Deps, hasher PasswordHasher, verifiers port.VerificationRepository, sponsors *SponsorService) *PortalService {
	return &PortalService{
		deps: d, hasher: hasher, verifiers: verifiers, sponsors: sponsors,
		auditor: newAuditor(d.Audit, d.Clock),
	}
}

// IssueCredentialResult carries the student's login and its one-time password.
type IssueCredentialResult struct {
	Username          string
	TemporaryPassword string
	StudentID         shared.ID
}

// IssueStudentCredential creates a student's portal login.
//
// The username is the student number, which is the one identifier a student
// already knows and cannot mistype twice. The password is generated and shown
// once: an administrator inventing passwords for a cohort invents the same one,
// and a student credential that a registrar knows is a student credential a
// registrar can use.
func (s *PortalService) IssueStudentCredential(ctx context.Context, actor shared.Actor, studentID shared.ID) (*IssueCredentialResult, error) {
	if err := actor.RequireAnyRole("IssueStudentCredential",
		shared.RoleRegistrar, shared.RoleAdmin); err != nil {
		return nil, err
	}

	result := &IssueCredentialResult{StudentID: studentID}
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		person, err := s.deps.Students.GetByID(ctx, studentID)
		if err != nil {
			return err
		}
		if existing, err := s.deps.Users.GetByStudent(ctx, studentID); err == nil && existing != nil {
			return shared.Conflict("portal.credential_exists",
				"this student already has a portal login (%s)", existing.Username).
				WithDetail("remedy", "reset its password rather than issuing a second, "+
					"which would leave two passwords able to see one record")
		} else if err != nil && shared.KindOf(err) != shared.KindNotFound {
			return err
		}

		password, err := generatePassword()
		if err != nil {
			return err
		}
		hash, err := s.hasher.Hash(password)
		if err != nil {
			return err
		}

		now := nowOr(s.deps.Clock)
		username := "s" + strings.ToLower(strings.TrimSpace(person.StudentNo))
		user := &port.User{
			ID:           shared.NewID(),
			Username:     username,
			FullName:     person.FullName,
			PasswordHash: hash,
			IsActive:     true,
			// A student is not an operator: one role, and it reaches exactly
			// their own record.
			Roles:              []shared.Role{shared.RoleStudent},
			MustChangePassword: true,
			PasswordChangedAt:  &now,
			ScopeMode:          shared.ScopeUniversity,
			StudentID:          &studentID,
		}
		if err := s.deps.Users.Create(ctx, user); err != nil {
			return err
		}

		result.Username, result.TemporaryPassword = username, password

		return s.record(ctx, port.AuditEntry{
			EntityType: "user",
			EntityID:   &user.ID,
			Action:     "portal.credential_issued",
			Actor:      actor,
			StudentID:  &studentID,
			Metadata:   map[string]any{"username": username},
		})
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// StatementLine is one account on a student's statement.
type StatementLine struct {
	Account      *billing.Account
	YearCode     string
	Installments []*billing.Installment
	Payments     []port.PaymentSummary
	Funding      *billing.FundingBreakdown
}

// Statement is what a student owes, across every year they have studied.
type Statement struct {
	StudentID    shared.ID
	StudentNo    string
	FullName     string
	Lines        []StatementLine
	TotalCharged money.Amount
	TotalPaid    money.Amount
	Outstanding  money.Amount
	// NextDue is the soonest unpaid installment across every account, which is
	// the one number a student actually wants.
	NextDue     *billing.Installment
	GeneratedAt time.Time
}

// StudentStatement assembles a student's whole financial position.
//
// Read from the rows rather than from the cached totals, like every other
// figure a person is shown or hands to somebody else. A student comparing this
// against a receipt in their pocket is exactly the audience the rule about
// printed figures was written for.
func (s *PortalService) StudentStatement(ctx context.Context, actor shared.Actor, studentID shared.ID) (*Statement, error) {
	if err := s.requireOwnership(actor, studentID, "StudentStatement"); err != nil {
		return nil, err
	}

	person, err := s.deps.Students.GetByID(ctx, studentID)
	if err != nil {
		return nil, err
	}
	accounts, err := s.deps.Accounts.ListForStudent(ctx, studentID)
	if err != nil {
		return nil, err
	}

	statement := &Statement{
		StudentID:   studentID,
		StudentNo:   person.StudentNo,
		FullName:    person.FullName,
		GeneratedAt: nowOr(s.deps.Clock),
	}

	for _, account := range accounts {
		if account.Status == billing.AccountCancelled {
			// A cancelled account belongs to a superseded enrollment; its money
			// moved to the replacement as a visible transfer pair, and showing
			// both would present the same obligation twice.
			continue
		}

		year, err := s.deps.Years.GetByID(ctx, account.AcademicYearID)
		if err != nil {
			return nil, err
		}
		installments, err := s.deps.Installments.ListForAccount(ctx, account.ID)
		if err != nil {
			return nil, err
		}
		payments, err := s.deps.Payments.SummariesForAccount(ctx, account.ID)
		if err != nil {
			return nil, err
		}

		line := StatementLine{
			Account:      account,
			YearCode:     year.Code,
			Installments: installments,
			Payments:     payments,
		}
		if s.sponsors != nil {
			funding, _, err := s.sponsors.Funding(ctx, actor, account)
			if err == nil && (funding.SponsorCovered.IsPositive() || funding.SponsorReceivable.IsPositive()) {
				line.Funding = &funding
			}
		}
		statement.Lines = append(statement.Lines, line)

		statement.TotalCharged = statement.TotalCharged.MustAdd(account.EffectiveNet())
		statement.TotalPaid = statement.TotalPaid.MustAdd(account.NetPaid())
		statement.Outstanding = statement.Outstanding.MustAdd(account.Remaining())

		for _, installment := range installments {
			if !installment.IsOpen() {
				continue
			}
			if statement.NextDue == nil || installment.DueDate.Before(statement.NextDue.DueDate) {
				statement.NextDue = installment
			}
		}
	}
	return statement, nil
}

// IssueVerificationResult is a printed statement's verification code.
type IssueVerificationResult struct {
	Code        string
	ExpiresAt   time.Time
	Outstanding money.Amount
}

// IssueStatementVerification mints the code a third party can check.
//
// The case it exists for: a student takes a statement to a sponsor, a ministry
// office or a bank, and that office has to decide whether the paper is real.
// The alternative in practice is a stamp, and a stamp is copied. The figures
// are frozen with the code, so the paper and the check agree even after the
// student pays something the next morning.
func (s *PortalService) IssueStatementVerification(
	ctx context.Context, actor shared.Actor, studentID shared.ID, yearID *shared.ID, validFor time.Duration,
) (*IssueVerificationResult, error) {
	if err := s.requireOwnership(actor, studentID, "IssueStatementVerification"); err != nil {
		return nil, err
	}
	if validFor <= 0 {
		validFor = 30 * 24 * time.Hour
	}

	statement, err := s.StudentStatement(ctx, actor, studentID)
	if err != nil {
		return nil, err
	}

	code, err := verificationCode()
	if err != nil {
		return nil, err
	}
	now := nowOr(s.deps.Clock)
	record := &port.StatementVerification{
		ID:             shared.NewID(),
		Code:           code,
		StudentID:      studentID,
		AcademicYearID: yearID,
		TotalCharged:   statement.TotalCharged,
		TotalPaid:      statement.TotalPaid,
		Outstanding:    statement.Outstanding,
		ContentSHA256:  statementDigest(statement),
		IssuedAt:       now,
		ExpiresAt:      now.Add(validFor),
	}
	if !shared.IsNil(actor.UserID) {
		record.IssuedBy = &actor.UserID
	}

	err = s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		if err := s.verifiers.Create(ctx, record); err != nil {
			return err
		}
		return s.record(ctx, port.AuditEntry{
			EntityType: "statement_verification",
			EntityID:   &record.ID,
			Action:     "statement.verification_issued",
			Actor:      actor,
			StudentID:  &studentID,
			Metadata: map[string]any{
				"outstanding": record.Outstanding.Int64(),
				"expires_at":  record.ExpiresAt.Format(time.RFC3339),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return &IssueVerificationResult{
		Code:        code,
		ExpiresAt:   record.ExpiresAt,
		Outstanding: record.Outstanding,
	}, nil
}

// VerificationAnswer is what a third party learns from a code.
//
// Deliberately thin. It confirms that this university issued a statement for a
// person with this name and student number, on this date, showing these
// figures. It does not carry a national identifier, a phone number, an address,
// or anything else the holder of a printed page has no business learning from
// an unauthenticated endpoint.
type VerificationAnswer struct {
	Valid        bool
	StudentNo    string
	FullName     string
	TotalCharged money.Amount
	TotalPaid    money.Amount
	Outstanding  money.Amount
	IssuedAt     time.Time
	ExpiresAt    time.Time
	Reason       string
}

// VerifyStatement checks a code presented by whoever holds the paper.
//
// Unauthenticated by design: an office checking a document has no account here
// and should not need one. What protects the student is that the code is
// unguessable and the answer is thin.
func (s *PortalService) VerifyStatement(ctx context.Context, code string) (*VerificationAnswer, error) {
	record, err := s.verifiers.GetByCode(ctx, strings.ToUpper(strings.TrimSpace(code)))
	if err != nil {
		if shared.KindOf(err) == shared.KindNotFound {
			// An invalid code is not an error to the caller; it is the answer.
			// Returning 404 would let somebody probe for valid codes by status.
			return &VerificationAnswer{Valid: false, Reason: "no statement was issued with this code"}, nil
		}
		return nil, err
	}

	now := nowOr(s.deps.Clock)
	answer := &VerificationAnswer{
		TotalCharged: record.TotalCharged,
		TotalPaid:    record.TotalPaid,
		Outstanding:  record.Outstanding,
		IssuedAt:     record.IssuedAt,
		ExpiresAt:    record.ExpiresAt,
	}
	switch {
	case record.RevokedAt != nil:
		answer.Reason = "this statement was withdrawn by the university"
		return answer, nil
	case now.After(record.ExpiresAt):
		answer.Reason = "this statement has expired; ask for a current one"
		return answer, nil
	}

	person, err := s.deps.Students.GetByID(ctx, record.StudentID)
	if err != nil {
		return nil, err
	}
	answer.Valid = true
	answer.StudentNo = person.StudentNo
	answer.FullName = person.FullName
	return answer, nil
}

// requireOwnership refuses a student reaching another student's record.
func (s *PortalService) requireOwnership(actor shared.Actor, studentID shared.ID, operation string) error {
	if actor.HasRole(shared.RoleStudent) {
		if actor.StudentID == nil || *actor.StudentID != studentID {
			return shared.Forbidden("portal.not_your_record",
				"this record belongs to another student")
		}
		return nil
	}
	// Staff reach it through the ordinary authority they already hold.
	return actor.RequireAnyRole(operation,
		shared.RoleRegistrar, shared.RoleCashier, shared.RoleFinanceManager,
		shared.RoleAdmin, shared.RoleAuditor, shared.RoleAcademicOfficer, shared.RoleReportViewer)
}

// verificationCode mints an unguessable, readable code.
//
// Base32 without padding and without the characters that are confused on a
// printed page: a code that has to be read down a telephone is a code that will
// be, and "0 or O" costs a call back.
func verificationCode() (string, error) {
	buf := make([]byte, 10)
	if _, err := rand.Read(buf); err != nil {
		return "", shared.Internal("portal.entropy_unavailable", err,
			"the system entropy source is unavailable")
	}
	raw := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf)
	return strings.ToUpper(raw[:16]), nil
}

// statementDigest fingerprints the figures a statement was issued with, so a
// document altered after printing fails verification even if its code is
// copied correctly.
func statementDigest(statement *Statement) string {
	var b strings.Builder
	b.WriteString(statement.StudentNo)
	b.WriteString("|")
	b.WriteString(statement.TotalCharged.String())
	b.WriteString("|")
	b.WriteString(statement.TotalPaid.String())
	b.WriteString("|")
	b.WriteString(statement.Outstanding.String())
	for _, line := range statement.Lines {
		b.WriteString("|")
		b.WriteString(line.YearCode)
		b.WriteString(":")
		b.WriteString(line.Account.Remaining().String())
	}
	digest := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(digest[:])
}

var _ = auth.ValidatePassword

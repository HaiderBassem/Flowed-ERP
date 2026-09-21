package pg

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"flowed/internal/domain/shared"
)

// PostgreSQL SQLSTATE codes this system reacts to by name rather than by
// string matching on driver messages.
const (
	codeUniqueViolation     = "23505"
	codeForeignKeyViolation = "23503"
	codeCheckViolation      = "23514"
	codeNotNullViolation    = "23502"
	codeExclusionViolation  = "23P01"
	codeSerializationFail   = "40001"
	codeDeadlockDetected    = "40P01"
	codeLockNotAvailable    = "55P03"
	codeQueryCanceled       = "57014"
	codeInsufficientRes     = "53000"
	codeTooManyConnections  = "53300"
	codeRaiseException      = "P0001"
	codeReadOnlySQLTx       = "25006"
)

// constraintErrors maps database constraint names onto the domain errors they
// actually mean.
//
// The unique indexes in this schema are not incidental: each one encodes a
// business rule that cannot be checked reliably in application code under
// concurrency. When one fires, the user deserves the rule's own message, not
// "duplicate key value violates unique constraint".
var constraintErrors = map[string]func(detail string) *shared.Error{
	"uq_student_number": func(d string) *shared.Error {
		return shared.Conflict("student.duplicate_number",
			"a student with this university number already exists").WithDetail("db_detail", d)
	},
	"uq_academic_year_code": func(d string) *shared.Error {
		return shared.Conflict("academic_year.duplicate_code",
			"this academic year already exists").WithDetail("db_detail", d)
	},
	"uq_enrollment_sequence": func(d string) *shared.Error {
		return shared.Conflict("enrollment.duplicate_sequence",
			"this enrollment sequence already exists for the student in this year").WithDetail("db_detail", d)
	},
	"uq_enrollment_one_live_per_year": func(d string) *shared.Error {
		return shared.Conflict("enrollment.already_enrolled",
			"the student already has a live enrollment in this academic year; supersede it instead of creating a second one").
			WithDetail("db_detail", d)
	},
	"uq_enrollment_superseded_by": func(d string) *shared.Error {
		return shared.Conflict("enrollment.supersede_chain_branch",
			"this enrollment is already superseded by another; the supersede chain cannot branch").WithDetail("db_detail", d)
	},
	"uq_financial_account_live_enrollment": func(d string) *shared.Error {
		return shared.Conflict("financial_account.already_exists",
			"this enrollment already has a financial account").WithDetail("db_detail", d)
	},
	"uq_fee_snapshot_component": func(d string) *shared.Error {
		return shared.Conflict("fee_snapshot.duplicate_component",
			"this fee component already appears in the account snapshot").WithDetail("db_detail", d)
	},
	"uq_fee_policy_scope": func(d string) *shared.Error {
		return shared.Conflict("fee_policy.duplicate_scope",
			"a published fee policy already covers exactly this scope; resolution must have a single winner").
			WithDetail("db_detail", d)
	},
	"uq_discount_definition_code": func(d string) *shared.Error {
		return shared.Conflict("discount.duplicate_definition_code",
			"a discount definition with this code already exists").WithDetail("db_detail", d)
	},
	"uq_discount_version": func(d string) *shared.Error {
		return shared.Conflict("discount.duplicate_version",
			"this discount definition version already exists").WithDetail("db_detail", d)
	},
	"uq_discount_application_active": func(d string) *shared.Error {
		return shared.Conflict("discount.already_applied",
			"this discount assignment is already applied to this account").WithDetail("db_detail", d)
	},
	"uq_installment_number": func(d string) *shared.Error {
		return shared.Conflict("installment.duplicate_number",
			"this installment number already exists on the account").WithDetail("db_detail", d)
	},
	"uq_payment_idempotency_key": func(d string) *shared.Error {
		return shared.Conflict("payment.duplicate_idempotency_key",
			"a payment with this idempotency key was already recorded").WithDetail("db_detail", d)
	},
	"uq_payment_receipt": func(d string) *shared.Error {
		return shared.Conflict("payment.duplicate_receipt_number",
			"this receipt number is already used in this series").WithDetail("db_detail", d)
	},
	"uq_refund_idempotency_key": func(d string) *shared.Error {
		return shared.Conflict("refund.duplicate_idempotency_key",
			"a refund with this idempotency key was already recorded").WithDetail("db_detail", d)
	},
	"uq_refund_receipt": func(d string) *shared.Error {
		return shared.Conflict("refund.duplicate_receipt_number",
			"this refund number is already used in this series").WithDetail("db_detail", d)
	},
	"uq_cashier_session_open": func(d string) *shared.Error {
		return shared.Conflict("cashier_session.already_open",
			"this cashier already has an open session; close it before opening another").WithDetail("db_detail", d)
	},
	"uq_number_series_scope": func(d string) *shared.Error {
		return shared.Conflict("number_series.duplicate_scope",
			"a number series already exists for this year and desk").WithDetail("db_detail", d)
	},
	"uq_idempotency_key": func(d string) *shared.Error {
		return shared.Conflict("command.duplicate_idempotency_key",
			"this command was already submitted with the same idempotency key").WithDetail("db_detail", d)
	},
	"uq_user_username": func(d string) *shared.Error {
		return shared.Conflict("user.duplicate_username", "this username is taken").WithDetail("db_detail", d)
	},
}

// TranslateError converts a driver error into a domain error, preserving the
// original as the wrapped cause so logs keep the full detail while the client
// sees a stable code.
func TranslateError(err error) error {
	if err == nil {
		return nil
	}

	// A domain error travelling back up through a repository is already in the
	// right shape; do not re-wrap it.
	if _, ok := shared.AsDomain(err); ok {
		return err
	}

	if errors.Is(err, pgx.ErrNoRows) {
		return shared.NotFound("not_found", "the requested record does not exist").WithCause(err)
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return shared.Internal("database_error", err, "database operation failed")
	}

	switch pgErr.Code {
	case codeUniqueViolation:
		if build, ok := constraintErrors[pgErr.ConstraintName]; ok {
			return build(pgErr.Detail).WithCause(err)
		}
		return shared.Conflict("duplicate_record",
			"a record violating uniqueness constraint %q already exists", pgErr.ConstraintName).
			WithDetail("constraint", pgErr.ConstraintName).
			WithDetail("db_detail", pgErr.Detail).
			WithCause(err)

	case codeForeignKeyViolation:
		return shared.Validation("invalid_reference",
			"a referenced record does not exist or is still referenced elsewhere (constraint %q)", pgErr.ConstraintName).
			WithDetail("constraint", pgErr.ConstraintName).
			WithDetail("db_detail", pgErr.Detail).
			WithCause(err)

	case codeCheckViolation:
		return shared.Validation("check_violation",
			"the value breaks database rule %q", pgErr.ConstraintName).
			WithDetail("constraint", pgErr.ConstraintName).
			WithDetail("db_detail", pgErr.Detail).
			WithCause(err)

	case codeNotNullViolation:
		return shared.Validation("missing_required_field",
			"column %q on table %q requires a value", pgErr.ColumnName, pgErr.TableName).
			WithDetail("column", pgErr.ColumnName).
			WithCause(err)

	case codeExclusionViolation:
		return shared.Conflict("overlapping_record",
			"the value overlaps an existing record (constraint %q)", pgErr.ConstraintName).
			WithDetail("constraint", pgErr.ConstraintName).
			WithCause(err)

	case codeSerializationFail:
		return shared.Conflict("serialization_failure",
			"the operation conflicted with a concurrent transaction; retry it").WithCause(err)

	case codeDeadlockDetected:
		return shared.Conflict("deadlock_detected",
			"the operation deadlocked with a concurrent transaction; retry it").WithCause(err)

	case codeLockNotAvailable:
		return shared.Conflict("record_locked",
			"another operation is currently modifying this record; retry shortly").WithCause(err)

	case codeQueryCanceled:
		return shared.PreconditionFailed("statement_timeout",
			"the operation exceeded its time budget and was cancelled").WithCause(err)

	case codeReadOnlySQLTx:
		return shared.InvariantViolation("write_in_read_only_transaction",
			"a write was attempted inside a read-only transaction").WithCause(err)

	case codeInsufficientRes, codeTooManyConnections:
		return shared.Internal("database_unavailable", err, "the database is out of resources")

	case codeRaiseException:
		// Raised by a trigger or a PL/pgSQL guard. These carry a deliberate
		// operator-facing message, so pass it through rather than flattening it.
		return shared.PreconditionFailed("database_rule_violation", "%s", pgErr.Message).
			WithDetail("hint", pgErr.Hint).
			WithCause(err)
	}

	return shared.Internal("database_error", err, "database operation failed with SQLSTATE %s", pgErr.Code)
}

// isRetryable reports whether an error is a transient conflict worth retrying.
func isRetryable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case codeSerializationFail, codeDeadlockDetected, codeLockNotAvailable:
			return true
		}
	}
	// Domain errors carry the same signal after translation.
	if domainErr, ok := shared.AsDomain(err); ok {
		switch domainErr.Code {
		case "serialization_failure", "deadlock_detected", "record_locked":
			return true
		}
	}
	return false
}

// IsUniqueViolation reports whether the error is a unique-constraint breach on
// the named constraint. Services use this for the rare case where losing a
// uniqueness race is a normal outcome rather than an error — for instance an
// idempotent insert that another replica just performed.
func IsUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	if pgErr.Code != codeUniqueViolation {
		return false
	}
	return constraint == "" || strings.EqualFold(pgErr.ConstraintName, constraint)
}

// IsNotFound reports whether the error means no row matched.
func IsNotFound(err error) bool {
	if errors.Is(err, pgx.ErrNoRows) {
		return true
	}
	return shared.KindOf(err) == shared.KindNotFound
}

// WrapQuery annotates a driver error with the operation that produced it,
// keeping repository errors readable in logs without leaking SQL to clients.
func WrapQuery(operation string, err error) error {
	if err == nil {
		return nil
	}
	translated := TranslateError(err)
	if domainErr, ok := shared.AsDomain(translated); ok {
		return domainErr.WithDetail("operation", operation)
	}
	return fmt.Errorf("%s: %w", operation, translated)
}

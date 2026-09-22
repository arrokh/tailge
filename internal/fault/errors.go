package fault

import "errors"

type ErrorCode string

const (
	ErrInvalidInput ErrorCode = "invalid_input"
	ErrDependency   ErrorCode = "dependency_unavailable"
	ErrPermission   ErrorCode = "permission_denied"
	ErrTimeout      ErrorCode = "timeout"
	ErrCancelled    ErrorCode = "cancelled"
	ErrOperation    ErrorCode = "operation_failed"
	ErrVerification ErrorCode = "verification_failed"
	ErrConfig       ErrorCode = "config_failure"
	ErrInterrupted  ErrorCode = "interrupted"
	ErrUnknown      ErrorCode = "unknown"
	ErrUnsafe       ErrorCode = "unsafe_operation"
	ErrAmbiguous    ErrorCode = "ambiguous_target"
	ErrUnsupported  ErrorCode = "unsupported"
)

func (c ErrorCode) ExitCode() int {
	switch c {
	case ErrInvalidInput, ErrAmbiguous, ErrUnsupported:
		return 2
	case ErrDependency:
		return 3
	case ErrPermission:
		return 4
	case ErrTimeout, ErrCancelled:
		return 5
	case ErrOperation, ErrUnsafe:
		return 6
	case ErrVerification, ErrUnknown:
		return 7
	case ErrConfig:
		return 8
	case ErrInterrupted:
		return 130
	default:
		return 1
	}
}

type AppError struct {
	Code        ErrorCode `json:"code"`
	Source      string    `json:"source"`
	Message     string    `json:"message"`
	Retryable   bool      `json:"retryable"`
	State       string    `json:"state"`
	Remediation string    `json:"remediation"`
	Exit        int       `json:"-"`
	Cause       error     `json:"-"`
}

func (e *AppError) Error() string {
	if e == nil {
		return ""
	}
	if e.Source == "" {
		return e.Message
	}
	return e.Source + ": " + e.Message
}

func (e *AppError) Unwrap() error { return e.Cause }

func NewError(code ErrorCode, source, message string, retryable bool, state, remediation string) *AppError {
	return &AppError{Code: code, Source: source, Message: message, Retryable: retryable, State: state, Remediation: remediation, Exit: code.ExitCode()}
}

func WrapError(code ErrorCode, source, message string, retryable bool, state, remediation string, cause error) *AppError {
	e := NewError(code, source, message, retryable, state, remediation)
	e.Cause = cause
	return e
}

func AsAppError(err error) *AppError {
	if err == nil {
		return nil
	}
	var appErr *AppError
	if errors.As(err, &appErr) && appErr != nil {
		return appErr
	}
	return NewError(ErrUnknown, "tailge", err.Error(), false, "unknown", "Run the command again after reviewing the diagnostics.")
}

type SafeError struct {
	Code        ErrorCode `json:"code"`
	Source      string    `json:"source"`
	Message     string    `json:"message"`
	Retryable   bool      `json:"retryable"`
	State       string    `json:"state"`
	Remediation string    `json:"remediation"`
	ExitCode    int       `json:"exit_code,omitempty"`
}

func (e *AppError) Safe() SafeError {
	if e == nil {
		return SafeError{}
	}
	return SafeError{Code: e.Code, Source: e.Source, Message: e.Message, Retryable: e.Retryable, State: e.State, Remediation: e.Remediation, ExitCode: e.Exit}
}

package utils

import (
	"fmt"

	"github.com/iryonetwork/wwm/gen/auth/models"
)

// error codes
const (
	ErrNotFound    = "not_found"
	ErrServerError = "server_error"
	ErrBadRequest  = "bad_request"
	ErrForbidden   = "forbidden"
)

// Error wraps models.Error so it will implement error interface
type Error struct {
	e models.Error
}

func (err Error) Error() string {
	return err.e.Message
}

// Code returns errors code
func (err Error) Code() string {
	return err.e.Code
}

// Payload returns inner error
func (err Error) Payload() *models.Error {
	return &err.e
}

// NewError returns new Error object
func NewError(code, message string, a ...interface{}) Error {
	return Error{
		e: models.Error{
			Code:    code,
			Message: fmt.Sprintf(message, a...),
		},
	}
}

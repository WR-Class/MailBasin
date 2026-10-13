package protocol

import (
	"errors"

	"icloud-privacy-mail-v2/internal/domain"
)

type ICloudSession = domain.ICloudSession
type LoginState = domain.LoginState
type SessionCookie = domain.SessionCookie
type Mailbox = domain.Mailbox

const (
	LoginStateICloudWeb    = domain.LoginStateICloudWeb
	LoginStateAppleAccount = domain.LoginStateAppleAccount
	LoginStateICloudIMAP   = domain.LoginStateICloudIMAP
)

type Error struct {
	code      string
	message   string
	retryable bool
}

func (e Error) Error() string {
	return e.message
}

type codedError = Error

func errCode(code, message string, retryable bool) error {
	return Error{code: code, message: message, retryable: retryable}
}

// NewCodedError 供其它包构造带错误码的错误，让上层能按码分支并保持 HTTP 状态正确。
func NewCodedError(code, message string, retryable bool) error {
	return errCode(code, message, retryable)
}

func ErrorDetails(err error) (code, message string, retryable bool) {
	var coded Error
	if errors.As(err, &coded) {
		return coded.code, coded.message, coded.retryable
	}
	if err == nil {
		return "", "", false
	}
	return "protocol_error", err.Error(), false
}

// ErrorDetailsCode 只取错误码，供调用方按码分支处理。
func ErrorDetailsCode(err error) string {
	code, _, _ := ErrorDetails(err)
	return code
}

package agentpromo

import "errors"

var (
	ErrNoPolicy           = errors.New("agentpromo: policy not set for org")
	ErrUnknownAgent       = errors.New("agentpromo: unknown agent")
	ErrUnknownVersion     = errors.New("agentpromo: unknown version")
	ErrInvalidTransition  = errors.New("agentpromo: invalid state transition")
	ErrTooManyTools       = errors.New("agentpromo: config exceeds tool limit")
	ErrMissingPromptField = errors.New("agentpromo: required prompt field missing or empty")
	ErrWaitNotElapsed     = errors.New("agentpromo: review wait period not elapsed")
	ErrNoSlots            = errors.New("agentpromo: no deployment slot available")
	ErrSlotLimitBelowUsed = errors.New("agentpromo: slot limit below current usage")
	ErrStaleApproval      = errors.New("agentpromo: approval bound to stale policy generation")
)

package appwire

import "fmt"

type PskAuthStatus uint8

const (
	PskAuthStatus_Ok     PskAuthStatus = 0
	PskAuthStatus_BadPsk PskAuthStatus = 1
)

func (s PskAuthStatus) String() string {
	switch s {
	case PskAuthStatus_Ok:
		return "Ok"
	case PskAuthStatus_BadPsk:
		return "BadPsk"
	default:
		return fmt.Sprintf("PskAuthStatus(%d)", uint8(s))
	}
}

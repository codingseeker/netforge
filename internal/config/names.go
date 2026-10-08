package config

import (
	"strconv"

	"github.com/codingseeker/netforge/internal/apperr"
)

const (
	MaxNameLength      = 63
	MinExperimentIDLen = 8
)

func checkName(op, field, value string) []error {
	if value == "" {
		return []error{apperr.InvalidArgument(op, field+" must not be empty", nil)}
	}
	if len(value) > MaxNameLength {
		return []error{apperr.InvalidArgument(op, field+" exceeds the maximum length of "+itoa(MaxNameLength), nil)}
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '-':
			if i == 0 || i == len(value)-1 {
				return []error{apperr.InvalidArgument(op, field+" "+strconv.Quote(value)+" must not start or end with '-'", nil)}
			}
		default:
			return []error{apperr.InvalidArgument(op, field+" "+strconv.Quote(value)+" may only contain lowercase letters, digits and '-'", nil)}
		}
	}
	return nil
}

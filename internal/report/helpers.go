package report

import (
	"fmt"
	"strconv"
)

func sprintf(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}

func itoa(value int64) string {
	return strconv.FormatInt(value, 10)
}

func itoaU64(value uint64) string {
	return strconv.FormatUint(value, 10)
}

func percent(value float64) string {
	if value == 0 {
		return "0"
	}
	return strconv.FormatFloat(value, 'f', 1, 64)
}

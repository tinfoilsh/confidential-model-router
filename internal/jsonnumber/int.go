// Package jsonnumber converts JSON numbers without rounding or expanding exponents.
package jsonnumber

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Int reports whether number is exactly representable as an int. Work and
// storage are proportional to the literal's length, regardless of its exponent.
func Int(number json.Number) (int, bool) {
	s := string(number)
	if s == "" || strings.TrimSpace(s) != s || (s[0] != '-' && (s[0] < '0' || s[0] > '9')) || !json.Valid([]byte(s)) {
		return 0, false
	}
	sign := ""
	if s[0] == '-' {
		sign, s = "-", s[1:]
	}
	mantissa, exponentText := s, "0"
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		mantissa, exponentText = s[:i], s[i+1:]
	}
	whole, fraction, _ := strings.Cut(mantissa, ".")
	digits := strings.TrimLeft(whole+fraction, "0")
	if digits == "" {
		return 0, true
	}
	trimmed := strings.TrimRight(digits, "0")
	decimalPlaces := len(fraction) - (len(digits) - len(trimmed))
	digits = trimmed

	// An int has at most 19 decimal digits, including on 64-bit platforms.
	const maxDigits = 19
	if len(digits) > maxDigits {
		return 0, false
	}
	exponent, err := strconv.Atoi(exponentText)
	if err != nil || exponent < decimalPlaces {
		return 0, false
	}
	// The difference is nonnegative. Unsigned subtraction avoids overflow
	// when an extreme positive exponent follows a long integer mantissa.
	zeros := uint(exponent) - uint(decimalPlaces)
	if zeros > uint(maxDigits-len(digits)) {
		return 0, false
	}
	n, err := strconv.Atoi(sign + digits + strings.Repeat("0", int(zeros)))
	return n, err == nil
}

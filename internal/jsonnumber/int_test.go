package jsonnumber

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"testing"
)

func TestIntExtremeExponents(t *testing.T) {
	for _, exponent := range []string{"1000000000", "-1000000000", "9223372036854775807", "-9223372036854775808", strings.Repeat("9", 1000)} {
		for _, mantissa := range []string{"1", "-1", "1000"} {
			if got, ok := Int(json.Number(mantissa + "e" + exponent)); ok {
				t.Fatalf("Int(%se%s) = %d, want rejection", mantissa, exponent, got)
			}
		}
		for _, mantissa := range []string{"0", "-0.000"} {
			if got, ok := Int(json.Number(mantissa + "e" + exponent)); !ok || got != 0 {
				t.Fatalf("Int(%se%s) = (%d, %v), want (0, true)", mantissa, exponent, got, ok)
			}
		}
	}
	// Long mantissas can cancel a large exponent without requiring big integers.
	for _, value := range []string{
		"42" + strings.Repeat("0", 4096) + "e-4096",
		"0." + strings.Repeat("0", 4096) + "42e4098",
	} {
		if got, ok := Int(json.Number(value)); !ok || got != 42 {
			t.Fatalf("cancelling exponent: got (%d, %v), want (42, true)", got, ok)
		}
	}
}

func TestIntRejectsInvalidNumbers(t *testing.T) {
	for _, value := range []string{"", "null", "true", `"42"`, "[]", "{}", " 42", "42 ", "+42", "01", "1.", ".1", "1e", "0e", "0e+", "1/1", "NaN", "Inf", "0x10"} {
		if got, ok := Int(json.Number(value)); ok {
			t.Errorf("Int(%q) = %d, want rejection", value, got)
		}
	}
}

func FuzzInt(f *testing.F) {
	f.Add(int64(42), uint64(0), int8(0))
	f.Add(int64(0), uint64(42), int8(19))
	f.Add(int64(math.MaxInt64), uint64(0), int8(0))
	f.Add(int64(math.MinInt64), uint64(0), int8(0))
	f.Add(int64(1), uint64(1), int8(0))
	f.Fuzz(func(t *testing.T, whole int64, fraction uint64, exponent int8) {
		// Keep the oracle's exponent bounded; only the production parser receives
		// the extreme exponents tested above.
		value := fmt.Sprintf("%d.%019de%d", whole, fraction, exponent)
		exact, ok := new(big.Rat).SetString(value)
		if !ok {
			t.Fatalf("invalid generated number %q", value)
		}
		wantOK := exact.IsInt() && exact.Num().IsInt64()
		want := 0
		if wantOK {
			var err error
			want, err = strconv.Atoi(exact.Num().String())
			wantOK = err == nil
		}
		got, gotOK := Int(json.Number(value))
		if gotOK != wantOK || (gotOK && got != want) {
			t.Fatalf("Int(%q) = (%d, %v), want (%d, %v)", value, got, gotOK, want, wantOK)
		}
	})
}

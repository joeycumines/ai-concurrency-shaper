package transcode

import (
	"encoding/json"
	"math/big"
	"math/rand"
	"strings"
	"testing"
)

// The oracle compares decimal mantissas as exact fractions and uses big.Int
// only to subtract their written exponents. It never constructs the value of
// a number with an enormous exponent. Unequal exponents outside the combined
// mantissa-length bound cannot be balanced by either finite mantissa.
func rationalNumberOracle(t *testing.T, a, b string) bool {
	t.Helper()
	parse := func(s string) (*big.Rat, *big.Int, int) {
		if !json.Valid([]byte(s)) {
			t.Fatalf("invalid generated number %q", s)
		}
		mantissa, exponent := s, "0"
		if i := strings.IndexAny(s, "eE"); i >= 0 {
			mantissa, exponent = s[:i], s[i+1:]
		}
		m, ok := new(big.Rat).SetString(mantissa)
		if !ok {
			t.Fatalf("invalid mantissa %q", mantissa)
		}
		e, ok := new(big.Int).SetString(exponent, 10)
		if !ok {
			t.Fatalf("invalid exponent %q", exponent)
		}
		return m, e, len(mantissa)
	}
	am, ae, al := parse(a)
	bm, be, bl := parse(b)
	if am.Sign() == 0 || bm.Sign() == 0 {
		return am.Sign() == bm.Sign()
	}
	if am.Sign() != bm.Sign() {
		return false
	}
	delta := new(big.Int).Sub(ae, be)
	bound := big.NewInt(int64(al + bl))
	if new(big.Int).Abs(delta).Cmp(bound) > 0 {
		return false
	}
	negative := delta.Sign() < 0
	power := new(big.Int).Exp(big.NewInt(10), new(big.Int).Abs(delta), nil)
	if negative {
		bm.Mul(bm, new(big.Rat).SetInt(power))
	} else {
		am.Mul(am, new(big.Rat).SetInt(power))
	}
	return am.Cmp(bm) == 0
}

func TestNumberLexemeComparisonAgainstRationalOracle(t *testing.T) {
	decimal := func(digits string, fractional int) string {
		if fractional == 0 {
			return digits
		}
		if fractional >= len(digits) {
			return "0." + strings.Repeat("0", fractional-len(digits)) + digits
		}
		return digits[:len(digits)-fractional] + "." + digits[len(digits)-fractional:]
	}
	exponents := []string{
		"0", "1", "-1", "9", "-9", "10", "-10", "99", "-99", "100", "-100",
		"1999999999", "-1999999999", "2000000000", "-2000000000",
		"2147483647", "2147483648", "-2147483647", "-2147483648",
		"9223372036854775807", "9223372036854775808", "-9223372036854775808",
		strings.Repeat("9", 64), "1" + strings.Repeat("0", 64),
		"-" + strings.Repeat("9", 64), "-1" + strings.Repeat("0", 64),
	}
	random := rand.New(rand.NewSource(71))
	check := func(a, b string) {
		t.Helper()
		want := rationalNumberOracle(t, a, b)
		if got := equalJSONNumberLexemes(a, b); got != want {
			t.Fatalf("equal(%q, %q) = %v; exact rational oracle = %v", a, b, got, want)
		}
		if got := equalJSONNumberLexemes(b, a); got != want {
			t.Fatalf("equal(%q, %q) = %v; exact rational oracle = %v", b, a, got, want)
		}
	}
	for i := 0; i < 2400; i++ {
		digits := []byte{byte('1' + random.Intn(9))}
		for range random.Intn(28) {
			digits = append(digits, byte('0'+random.Intn(10)))
		}
		if i%9 == 0 {
			digits = []byte("0")
		}
		coefficient := string(digits)
		fractional := random.Intn(len(digits) + 5)
		exponent := exponents[random.Intn(len(exponents))]
		sign := ""
		if random.Intn(2) == 0 {
			sign = "-"
		}
		a := sign + decimal(coefficient, fractional) + "e" + exponent
		zeros := random.Intn(40)
		shifted := coefficient
		if coefficient != "0" {
			shifted += strings.Repeat("0", zeros)
		}
		otherFractional := random.Intn(len(shifted) + 5)
		e, _ := new(big.Int).SetString(exponent, 10)
		e.Add(e, big.NewInt(int64(otherFractional-fractional-zeros)))
		otherExponent := e.String()
		if e.Sign() < 0 {
			otherExponent = "-000" + otherExponent[1:]
		} else {
			otherExponent = "+000" + otherExponent
		}
		b := sign + decimal(shifted, otherFractional) + "E" + otherExponent
		check(a, b)
		mutated, _ := new(big.Int).SetString(shifted, 10)
		mutated.Add(mutated, big.NewInt(1))
		check(a, sign+decimal(mutated.String(), otherFractional)+"E"+otherExponent)
		check(a, "-"+strings.TrimPrefix(b, "-"))
	}
	for _, exponent := range []string{
		strings.Repeat("9", 8192), "1" + strings.Repeat("0", 8192),
		"-" + strings.Repeat("9", 8192), "-1" + strings.Repeat("0", 8192),
	} {
		e, _ := new(big.Int).SetString(exponent, 10)
		e.Sub(e, big.NewInt(1))
		check("1e"+exponent, "10e"+e.String())
		check("1e"+exponent, "11e"+e.String())
	}
}

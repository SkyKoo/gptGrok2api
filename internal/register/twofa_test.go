package register

import (
	"testing"
	"time"
)

func TestTOTPCodeRFC6238(t *testing.T) {
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	for _, test := range []struct {
		at   int64
		code string
	}{{59, "287082"}, {1111111109, "081804"}, {1111111111, "050471"}, {1234567890, "005924"}, {2000000000, "279037"}, {20000000000, "353130"}} {
		got, err := TOTPCode(secret, time.Unix(test.at, 0))
		if err != nil || got != test.code {
			t.Fatalf("at %d: got %q %v", test.at, got, err)
		}
	}
	for _, secret := range []string{"", "invalid-secret!", "AAAA"} {
		if _, err := TOTPCode(secret, time.Now()); err == nil {
			t.Fatal("accepted invalid secret")
		}
	}
}

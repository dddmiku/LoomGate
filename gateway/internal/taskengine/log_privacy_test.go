package taskengine

import "testing"

func TestShortAccountMasksNumericIdentifiers(t *testing.T) {
	cases := map[string]string{
		"13800138000":     "138****8000",
		"1234567":         "1*****7",
		"1234567890":      "1********0",
		"123456789012345": "123********2345",
	}
	for input, want := range cases {
		if got := shortAccount(input); got != want {
			t.Errorf("shortAccount(%q)=%q want %q", input, got, want)
		}
	}
	if got := shortAccount("account-1234567890"); got != "account" {
		t.Fatalf("non-numeric account label changed: %q", got)
	}
}

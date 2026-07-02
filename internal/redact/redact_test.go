package redact

import (
	"strings"
	"testing"
)

func TestDisabledIsNoop(t *testing.T) {
	r := New(false, nil)
	out, counts := r.Redact("email me at a@b.com")
	if out != "email me at a@b.com" || counts != nil {
		t.Fatalf("disabled should be a no-op, got %q %v", out, counts)
	}
}

func TestBuiltins(t *testing.T) {
	r := New(true, nil)
	in := "mail a@b.com, ssn 123-45-6789, key sk-ant-abcdefghijklmnop123, card 4111 1111 1111 1111"
	out, counts := r.Redact(in)
	for _, want := range []string{"[REDACTED_EMAIL]", "[REDACTED_SSN]", "[REDACTED_API_KEY]", "[REDACTED_CC]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %s in output, got %q", want, out)
		}
	}
	if strings.Contains(out, "a@b.com") || strings.Contains(out, "123-45-6789") || strings.Contains(out, "sk-ant-") {
		t.Fatalf("original PII leaked: %q", out)
	}
	if counts["EMAIL"] != 1 || counts["SSN"] != 1 || counts["API_KEY"] != 1 {
		t.Fatalf("bad counts: %v", counts)
	}
}

func TestCustomPattern(t *testing.T) {
	r := New(true, map[string]string{"TICKET": `JIRA-\d+`})
	out, counts := r.Redact("see JIRA-1234 for details")
	if !strings.Contains(out, "[REDACTED_TICKET]") || counts["TICKET"] != 1 {
		t.Fatalf("custom rule not applied: %q %v", out, counts)
	}
}

func TestCreditCardLuhnGate(t *testing.T) {
	r := New(true, nil)
	// Valid Luhn (test Visa) → redacted.
	if out, c := r.Redact("card 4111111111111111 end"); !strings.Contains(out, "[REDACTED_CC]") || c["CREDIT_CARD"] != 1 {
		t.Fatalf("valid card should be redacted: %q %v", out, c)
	}
	// 13-digit order id that fails Luhn → left alone (no false positive).
	if out, c := r.Redact("order 1234567890123 end"); strings.Contains(out, "[REDACTED_CC]") || c["CREDIT_CARD"] != 0 {
		t.Fatalf("non-Luhn digit run must not be redacted: %q %v", out, c)
	}
}

func TestSecretFamilies(t *testing.T) {
	r := New(true, nil)
	cases := map[string]string{
		"AKIAIOSFODNN7EXAMPLE":                                              "[REDACTED_AWS_KEY]",
		"ghp_abcdefghijklmnopqrstuvwxyz0123456789":                          "[REDACTED_GITHUB_TOKEN]",
		"-----BEGIN RSA PRIVATE KEY-----":                                   "[REDACTED_PRIVATE_KEY]",
		"eyJhbGciOi.eyJzdWIiOaa.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJVadQssw5c": "[REDACTED_JWT]",
	}
	for in, want := range cases {
		if out, _ := r.Redact(in); !strings.Contains(out, want) {
			t.Fatalf("expected %s for %q, got %q", want, in, out)
		}
	}
}

func TestValidatePatterns(t *testing.T) {
	if err := ValidatePatterns(map[string]string{"ok": `\d+`}); err != nil {
		t.Fatalf("valid pattern rejected: %v", err)
	}
	if err := ValidatePatterns(map[string]string{"bad": `[`}); err == nil {
		t.Fatal("invalid pattern should error")
	}
}

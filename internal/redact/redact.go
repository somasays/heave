// Package redact provides a best-effort, regex-based PII/secret scrubber applied
// to request content before it reaches a vendor (a pre-flight hook). It is pure
// (stdlib regexp only) and deterministic.
//
// This is regex redaction, not ML PII detection: it catches common structured
// identifiers (email, credit card, US SSN, phone, API keys) and any operator-
// supplied patterns. It is intentionally conservative — redaction is lossy and
// can change model behavior, so it is opt-in via config.
package redact

import (
	"regexp"
	"sort"
)

type rule struct {
	name        string
	re          *regexp.Regexp
	placeholder string
	// validate, when non-nil, gates a regex match: the match is only redacted
	// when validate returns true (e.g. Luhn for credit cards), cutting false
	// positives on look-alike digit runs.
	validate func(string) bool
}

// Redactor applies a set of redaction rules.
type Redactor struct {
	enabled bool
	rules   []rule
}

// builtinRules are the default detectors. Ordered most-specific first (secrets
// before looser structural patterns) so a token isn't partially caught by a
// looser rule. Regexes are RE2 (linear time) — no ReDoS risk.
func builtinRules() []rule {
	return []rule{
		{"PRIVATE_KEY", regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH |PGP )?PRIVATE KEY-----`), "[REDACTED_PRIVATE_KEY]", nil},
		{"JWT", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.eyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\b`), "[REDACTED_JWT]", nil},
		{"AWS_ACCESS_KEY", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), "[REDACTED_AWS_KEY]", nil},
		{"GCP_API_KEY", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`), "[REDACTED_GCP_KEY]", nil},
		{"GITHUB_TOKEN", regexp.MustCompile(`\bgh[posru]_[A-Za-z0-9]{20,}\b`), "[REDACTED_GITHUB_TOKEN]", nil},
		{"SLACK_TOKEN", regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`), "[REDACTED_SLACK_TOKEN]", nil},
		{"API_KEY", regexp.MustCompile(`\b(?:sk-ant-[A-Za-z0-9_-]{16,}|sk-[A-Za-z0-9]{16,})\b`), "[REDACTED_API_KEY]", nil},
		{"EMAIL", regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`), "[REDACTED_EMAIL]", nil},
		{"SSN", regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`), "[REDACTED_SSN]", nil},
		{"CREDIT_CARD", regexp.MustCompile(`\b(?:\d[ -]?){13,19}\b`), "[REDACTED_CC]", luhnValid},
		{"PHONE", regexp.MustCompile(`\b(?:\+?\d{1,2}[ .\-]?)?(?:\(\d{3}\)|\d{3})[ .\-]?\d{3}[ .\-]?\d{4}\b`), "[REDACTED_PHONE]", nil},
	}
}

// luhnValid reports whether the digits in s pass the Luhn checksum (13–19
// digits). Used to gate credit-card redaction so ordinary digit runs (order
// ids, quantities) are not scrubbed.
func luhnValid(s string) bool {
	var digits []int
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits = append(digits, int(r-'0'))
		}
	}
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := digits[i]
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

// New builds a Redactor. When enabled is false, Redact is a no-op. customPatterns
// maps a rule name to a regexp; invalid patterns are skipped (caller should
// validate at config load). Custom rules run after the built-ins.
func New(enabled bool, customPatterns map[string]string) *Redactor {
	r := &Redactor{enabled: enabled}
	if !enabled {
		return r
	}
	r.rules = builtinRules()
	// Deterministic order for custom rules.
	names := make([]string, 0, len(customPatterns))
	for n := range customPatterns {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if re, err := regexp.Compile(customPatterns[n]); err == nil {
			r.rules = append(r.rules, rule{name: n, re: re, placeholder: "[REDACTED_" + n + "]"})
		}
	}
	return r
}

// Enabled reports whether redaction will do anything.
func (r *Redactor) Enabled() bool { return r.enabled }

// Redact returns the scrubbed string and a count of replacements per rule name.
// The counts never include the redacted values themselves (safe to log).
func (r *Redactor) Redact(s string) (string, map[string]int) {
	if !r.enabled || s == "" {
		return s, nil
	}
	var counts map[string]int
	for _, ru := range r.rules {
		n := 0
		s = ru.re.ReplaceAllStringFunc(s, func(match string) string {
			if ru.validate != nil && !ru.validate(match) {
				return match // look-alike (e.g. non-Luhn digit run) — leave as is
			}
			n++
			return ru.placeholder
		})
		if n > 0 {
			if counts == nil {
				counts = map[string]int{}
			}
			counts[ru.name] += n
		}
	}
	return s, counts
}

// ValidatePatterns reports the first invalid custom pattern, or nil. Used at
// config load so a bad regex fails fast instead of being silently skipped.
func ValidatePatterns(customPatterns map[string]string) error {
	names := make([]string, 0, len(customPatterns))
	for n := range customPatterns {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if _, err := regexp.Compile(customPatterns[n]); err != nil {
			return err
		}
	}
	return nil
}

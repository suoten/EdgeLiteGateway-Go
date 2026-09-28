package models

import "testing"

func TestValidatePassword(t *testing.T) {
	// Valid passwords
	valid := []string{"Passw0rd!", "Abc123@#$", "Secure!1"}
	for _, pw := range valid {
		if !ValidatePassword(pw) {
			t.Errorf("Password '%s' should be valid", pw)
		}
	}

	// Invalid passwords
	invalid := []string{"short", "allletters", "12345678", "noSpecial12", "", "abcdefgh1"} // last has no special char
	for _, pw := range invalid {
		if ValidatePassword(pw) {
			t.Errorf("Password '%s' should be invalid", pw)
		}
	}

	// Too long
	longPw := ""
	for i := 0; i < 73; i++ {
		longPw += "a"
	}
	if ValidatePassword(longPw) {
		t.Error("Password longer than 72 chars should be invalid")
	}
}

func TestValidateUsername(t *testing.T) {
	// Valid usernames
	valid := []string{"admin", "user123", "test_user", "ABC"}
	for _, u := range valid {
		if !ValidateUsername(u) {
			t.Errorf("Username '%s' should be valid", u)
		}
	}

	// Invalid usernames
	invalid := []string{"ab", "user@name", "user.name", "", "a", "very_long_username_that_exceeds_thirty_two_chars_limit"}
	for _, u := range invalid {
		if ValidateUsername(u) {
			t.Errorf("Username '%s' should be invalid", u)
		}
	}
}

func TestValidRuleTypes(t *testing.T) {
	if len(ValidRuleTypes) != 3 {
		t.Fatalf("Expected 3 valid rule types, got %d", len(ValidRuleTypes))
	}

	for _, rt := range ValidRuleTypes {
		if !isValidRuleType(rt) {
			t.Errorf("Rule type '%s' should be valid", rt)
		}
	}

	if isValidRuleType("invalid_type") {
		t.Error("invalid_type should not be a valid rule type")
	}
}

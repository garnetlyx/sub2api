package kiro

import "testing"

func TestExtractSubjectFromAccessToken(t *testing.T) {
	token := "header.eyJzdWIiOiJidWlsZGVyLTEyMyJ9.signature"
	if got := ExtractSubjectFromAccessToken(token); got != "builder-123" {
		t.Fatalf("expected subject builder-123, got %q", got)
	}
}

func TestExtractSubjectFromAccessTokenInvalid(t *testing.T) {
	if got := ExtractSubjectFromAccessToken("not-a-jwt"); got != "" {
		t.Fatalf("expected empty subject, got %q", got)
	}
}

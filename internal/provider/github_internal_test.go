package provider

import "testing"

func TestGitHubDefaultBaseURL(t *testing.T) {
	gh := NewGitHub("", "tok")
	if gh.c.base != "https://api.github.com" {
		t.Fatalf("base = %q, want https://api.github.com", gh.c.base)
	}
}

func TestGitLabDefaultBaseURL(t *testing.T) {
	gl := NewGitLab("", "tok")
	if gl.c.base != "https://gitlab.com/api/v4" {
		t.Fatalf("base = %q, want https://gitlab.com/api/v4", gl.c.base)
	}
}

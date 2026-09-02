package provider

import (
	"net/http"
	"net/url"
	"testing"
)

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

// net/http strips the Authorization header only when a redirect crosses to a
// different domain, so a same-host https->http hop would otherwise carry the
// forge token onto a plaintext connection. This is the policy that stops it.
func TestRefuseInsecureRedirect(t *testing.T) {
	req := func(rawurl string) *http.Request {
		u, err := url.Parse(rawurl)
		if err != nil {
			t.Fatalf("parsing %q: %v", rawurl, err)
		}

		return &http.Request{URL: u}
	}

	cases := []struct {
		name    string
		to      string
		via     []string
		wantErr bool
	}{
		{name: "https to https", to: "https://api.github.com/b", via: []string{"https://api.github.com/a"}},
		{name: "http to http stays allowed", to: "http://localhost/b", via: []string{"http://localhost/a"}},

		{
			name: "same host https to http", to: "http://api.github.com/b",
			via: []string{"https://api.github.com/a"}, wantErr: true,
		},
		{
			name: "downgrade to another host", to: "http://evil.example.com/b",
			via: []string{"https://api.github.com/a"}, wantErr: true,
		},
		{
			// The hop that downgrades may not be the first one.
			name: "downgrade later in the chain", to: "http://api.github.com/c",
			via:     []string{"https://api.github.com/a", "https://api.github.com/b"},
			wantErr: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			via := make([]*http.Request, 0, len(c.via))
			for _, v := range c.via {
				via = append(via, req(v))
			}

			err := refuseInsecureRedirect(req(c.to), via)
			if c.wantErr && err == nil {
				t.Fatalf("redirect to %s was allowed; the forge token would go out in cleartext", c.to)
			}

			if !c.wantErr && err != nil {
				t.Fatalf("redirect to %s refused: %v", c.to, err)
			}
		})
	}
}

func TestRefuseInsecureRedirectStopsARedirectLoop(t *testing.T) {
	u, err := url.Parse("https://api.github.com/a")
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}

	via := make([]*http.Request, 10)
	for i := range via {
		via[i] = &http.Request{URL: u}
	}

	if err := refuseInsecureRedirect(&http.Request{URL: u}, via); err == nil {
		t.Fatal("want the redirect chain to be capped")
	}
}

package reviewer

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestExtractJSON(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare", `{"a":1}`, `{"a":1}`},
		{"prose around", "Sure!\n{\"a\":1}\nHope that helps", `{"a":1}`},
		{"fenced", "```json\n{\"a\":1}\n```", `{"a":1}`},
		{"nested", `{"a":{"b":2}}`, `{"a":{"b":2}}`},
		{"brace in string", `{"body":"use } carefully"}`, `{"body":"use } carefully"}`},
		{"escaped quote", `{"body":"say \"}\" out loud"}`, `{"body":"say \"}\" out loud"}`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := extractJSON(c.in)
			if err != nil {
				t.Fatalf("extractJSON: %v", err)
			}

			if got != c.want {
				t.Fatalf("extractJSON = %q, want %q", got, c.want)
			}
		})
	}
}

func TestExtractJSONErrors(t *testing.T) {
	if _, err := extractJSON("no json at all"); err == nil {
		t.Fatal("want error for output with no object")
	}

	if _, err := extractJSON(`{"a": [1,2`); err == nil {
		t.Fatal("want error for unbalanced object")
	}
}

func TestParseResultDropsUnusableFindings(t *testing.T) {
	out := `noise
	{"summary":"looks risky","findings":[
	  {"file":"a.go","line":3,"severity":"critical","title":"nil deref","body":"guard it"},
	  {"file":"","line":3,"severity":"major","title":"no file","body":"x"},
	  {"file":"b.go","line":4,"severity":"","title":"unknown severity","body":"x"},
	  {"file":"c.go","line":5,"severity":"minor","title":"","body":"x"}
	]}`

	res, err := parseResult(out)
	if err != nil {
		t.Fatalf("parseResult: %v", err)
	}

	if res.Summary != "looks risky" {
		t.Fatalf("Summary = %q", res.Summary)
	}

	if len(res.Findings) != 2 {
		t.Fatalf("findings = %+v, want 2 (unanchorable ones dropped)", res.Findings)
	}

	if res.Findings[1].Severity != SeverityMinor {
		t.Fatalf("unknown severity = %q, want defaulted to minor", res.Findings[1].Severity)
	}
}

func TestParseResultRejectsInvalidJSON(t *testing.T) {
	if _, err := parseResult(`{"summary": 42}`); err == nil {
		t.Fatal("want decode error for wrong field type")
	}
}

func TestSeverityAtLeast(t *testing.T) {
	if !SeverityCritical.AtLeast(SeverityMinor) {
		t.Fatal("critical should pass a minor threshold")
	}

	if SeverityNit.AtLeast(SeverityMajor) {
		t.Fatal("nit should not pass a major threshold")
	}
}

func TestFingerprintIgnoresBodyWording(t *testing.T) {
	a := Finding{File: "a.go", Line: 3, Title: "Nil deref", Body: "first wording"}
	b := Finding{File: "A.GO", Line: 99, Title: "nil deref", Body: "reworded entirely"}

	if a.Fingerprint() != b.Fingerprint() {
		t.Fatal("same issue reworded produced a different fingerprint; it would be posted twice")
	}

	c := Finding{File: "a.go", Line: 3, Title: "other issue"}
	if a.Fingerprint() == c.Fingerprint() {
		t.Fatal("different issues collided")
	}
}

// The NUL-joined fingerprint is injective only while no field contains a NUL of
// its own. JSON can encode one, so an engine could otherwise emit a finding
// that fingerprints identically to a real one and silently suppress it.
func TestParseResultDropsFindingsCarryingNUL(t *testing.T) {
	// ("a.go" + NUL + "shadow", "t") and ("a.go", "shadow" + NUL + "t") join to
	// the same byte string, so the second would displace the first.
	out := `{"summary":"s","findings":[
	  {"file":"a.go\u0000shadow","line":1,"severity":"major","title":"t","body":"b"},
	  {"file":"a.go","line":2,"severity":"major","title":"shadow\u0000t","body":"b"},
	  {"file":"real.go","line":3,"severity":"major","title":"kept","body":"b"}]}`

	res, err := parseResult(out)
	if err != nil {
		t.Fatalf("parseResult: %v", err)
	}

	if len(res.Findings) != 1 {
		t.Fatalf("findings = %+v, want only the NUL-free one", res.Findings)
	}

	if res.Findings[0].File != "real.go" {
		t.Fatalf("kept %q, want real.go", res.Findings[0].File)
	}
}

type fakeEngine struct {
	name string
	res  Result
	err  error
	// calls counts invocations, to prove the chain stopped where expected.
	calls *int
}

func (f fakeEngine) Name() string { return f.name }

func (f fakeEngine) Review(_ context.Context, _ Request) (Result, error) {
	*f.calls++

	return f.res, f.err
}

func TestChainFallsBackOnlyOnRateLimit(t *testing.T) {
	var primary, fallback int

	chain := Chain{Engines: []Engine{
		fakeEngine{name: "claude", err: ErrRateLimited, calls: &primary},
		fakeEngine{name: "opencode", res: Result{Summary: "ok"}, calls: &fallback},
	}}

	res, err := chain.Review(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}

	if res.Engine != "opencode" {
		t.Fatalf("Engine = %q, want opencode", res.Engine)
	}

	if primary != 1 || fallback != 1 {
		t.Fatalf("calls: primary=%d fallback=%d, want 1 and 1", primary, fallback)
	}
}

func TestChainDoesNotFallBackOnOtherErrors(t *testing.T) {
	var primary, fallback int

	chain := Chain{Engines: []Engine{
		fakeEngine{name: "claude", err: errors.New("bad prompt"), calls: &primary},
		fakeEngine{name: "opencode", res: Result{Summary: "ok"}, calls: &fallback},
	}}

	if _, err := chain.Review(context.Background(), Request{}); err == nil {
		t.Fatal("want error")
	}

	if fallback != 0 {
		t.Fatal("fallback engine ran for a non-rate-limit failure and burned quota")
	}
}

func TestChainAllRateLimited(t *testing.T) {
	var a, b int

	chain := Chain{Engines: []Engine{
		fakeEngine{name: "claude", err: ErrRateLimited, calls: &a},
		fakeEngine{name: "opencode", err: ErrRateLimited, calls: &b},
	}}

	_, err := chain.Review(context.Background(), Request{})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}

	if chain.Name() != "claude->opencode" {
		t.Fatalf("Name = %q", chain.Name())
	}
}

func TestChainWithNoEngines(t *testing.T) {
	if _, err := (Chain{}).Review(context.Background(), Request{}); err == nil {
		t.Fatal("want error for empty chain")
	}
}

func TestBuildPromptCarriesScopeAndPriorFindings(t *testing.T) {
	got := buildPrompt(Request{
		Repo:          "acme/app",
		PRNumber:      7,
		Title:         "Add cache",
		Body:          "why not",
		Diff:          "@@ -1 +1 @@",
		Cycle:         2,
		PriorFindings: []string{"nil deref"},
	}, 25)

	for _, want := range []string{
		"acme/app",
		"#7",
		"Add cache",
		"why not",
		"nil deref",
		"pass 2 of 2",
		"@@ -1 +1 @@",
		"Max 25 findings",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt missing %q:\n%s", want, got)
		}
	}
}

func TestBuildPromptFirstCycleHasNoDeltaWording(t *testing.T) {
	got := buildPrompt(Request{Repo: "r", PRNumber: 1, Title: "t", Diff: "d", Cycle: 1}, 10)
	if strings.Contains(got, "pass 2 of 2") {
		t.Fatal("first cycle prompt claims to be the incremental pass")
	}
}

func TestExtractJSONSkipsProseBraces(t *testing.T) {
	got, err := extractJSON(`writing to {repo}/out ... {"summary":"s","findings":[]} done`)
	if err != nil {
		t.Fatalf("extractJSON: %v", err)
	}

	if got != `{"summary":"s","findings":[]}` {
		t.Fatalf("extractJSON = %q", got)
	}
}

func TestBuildPromptSecondCycleWithoutPriorFindingsHasNoDanglingList(t *testing.T) {
	got := buildPrompt(Request{Repo: "r", PRNumber: 1, Title: "t", Diff: "d", Cycle: 2}, 10)

	if !strings.Contains(got, "pass 2 of 2") {
		t.Fatalf("second cycle prompt lost its scope line:\n%s", got)
	}

	if strings.Contains(got, "the list above") {
		t.Fatalf("prompt points at a list that was never rendered:\n%s", got)
	}
}

// verifyingFake is a fakeEngine that also implements Verifier, so a chain can
// mix engines that can verify with ones that cannot.
type verifyingFake struct {
	fakeEngine

	vres VerifyResult
	verr error
}

func (f verifyingFake) Verify(_ context.Context, _ VerifyRequest) (VerifyResult, error) {
	*f.calls++

	return f.vres, f.verr
}

func TestChainVerifyFallsBackOnlyOnRateLimit(t *testing.T) {
	var primary, fallback int

	chain := Chain{Engines: []Engine{
		verifyingFake{fakeEngine: fakeEngine{name: "claude", calls: &primary}, verr: ErrRateLimited},
		verifyingFake{
			fakeEngine: fakeEngine{name: "opencode", calls: &fallback},
			vres:       VerifyResult{Verdicts: []ThreadVerdict{{ID: "T1", Verdict: VerdictFixed}}},
		},
	}}

	res, err := chain.Verify(context.Background(), VerifyRequest{Threads: []OpenThread{{ID: "T1"}}})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if res.Engine != "opencode" || len(res.Verdicts) != 1 {
		t.Fatalf("res = %+v, want the fallback's verdicts", res)
	}

	if primary != 1 || fallback != 1 {
		t.Fatalf("calls: primary=%d fallback=%d", primary, fallback)
	}
}

func TestChainVerifyDoesNotFallBackOnOtherErrors(t *testing.T) {
	var primary, fallback int

	chain := Chain{Engines: []Engine{
		verifyingFake{fakeEngine: fakeEngine{name: "claude", calls: &primary}, verr: errors.New("bad prompt")},
		verifyingFake{fakeEngine: fakeEngine{name: "opencode", calls: &fallback}},
	}}

	if _, err := chain.Verify(context.Background(), VerifyRequest{}); err == nil {
		t.Fatal("err = nil, want the failure returned without burning the fallback")
	}

	if fallback != 0 {
		t.Fatalf("fallback ran %d time(s) on a non-rate-limit failure", fallback)
	}
}

// Pairing a verifying engine with one that cannot verify must still work: the
// non-verifier is skipped, not treated as a failure.
func TestChainVerifySkipsEnginesThatCannotVerify(t *testing.T) {
	var plain, verifier int

	chain := Chain{Engines: []Engine{
		fakeEngine{name: "plain", calls: &plain},
		verifyingFake{
			fakeEngine: fakeEngine{name: "claude", calls: &verifier},
			vres:       VerifyResult{Verdicts: []ThreadVerdict{{ID: "T1", Verdict: VerdictPartial}}},
		},
	}}

	res, err := chain.Verify(context.Background(), VerifyRequest{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if res.Engine != "claude" {
		t.Fatalf("Engine = %q, want the verifying engine", res.Engine)
	}

	if plain != 0 {
		t.Errorf("the non-verifying engine was invoked %d time(s)", plain)
	}
}

func TestChainVerifyWithNoVerifyingEngines(t *testing.T) {
	var plain int

	chain := Chain{Engines: []Engine{fakeEngine{name: "plain", calls: &plain}}}

	if _, err := chain.Verify(context.Background(), VerifyRequest{}); err == nil {
		t.Fatal("err = nil, want an error when nothing in the chain can verify")
	}
}

package reviewer

import (
	"strings"
	"testing"
)

func askedThreads() []OpenThread {
	return []OpenThread{
		{ID: "T1", File: "a.ts", Line: 30, Finding: "array bodies skip trace_id", Replies: []string{"author: fixed"}},
		{ID: "T2", File: "b.ts", Line: 7, Finding: "missing test"},
	}
}

func TestParseVerifyResultKeepsOnlyThreadsThatWereAsked(t *testing.T) {
	out := `{"verdicts":[
	  {"id":"T1","verdict":"fixed","note":"ok"},
	  {"id":"GHOST","verdict":"fixed","note":"invented"},
	  {"id":"T2","verdict":"partial","note":"half"}
	]}`

	res, err := parseVerifyResult(out, askedThreads())
	if err != nil {
		t.Fatalf("parseVerifyResult: %v", err)
	}

	if len(res.Verdicts) != 2 {
		t.Fatalf("verdicts = %+v, want the invented thread dropped", res.Verdicts)
	}

	for _, v := range res.Verdicts {
		if v.ID == "GHOST" {
			t.Fatal("a hallucinated thread id survived parsing; it would resolve an unknown thread")
		}
	}
}

func TestParseVerifyResultDowngradesAnUnknownVerdict(t *testing.T) {
	res, err := parseVerifyResult(`{"verdicts":[{"id":"T1","verdict":"definitely-done","note":"n"}]}`, askedThreads())
	if err != nil {
		t.Fatalf("parseVerifyResult: %v", err)
	}

	if len(res.Verdicts) != 1 {
		t.Fatalf("verdicts = %+v", res.Verdicts)
	}

	if res.Verdicts[0].Verdict != VerdictUnrelated {
		t.Fatalf(
			"verdict = %q, want %q: an unrecognised verdict must never be read as fixed",
			res.Verdicts[0].Verdict, VerdictUnrelated,
		)
	}
}

func TestParseVerifyResultIgnoresARepeatedThread(t *testing.T) {
	out := `{"verdicts":[{"id":"T1","verdict":"unfixed","note":"a"},{"id":"T1","verdict":"fixed","note":"b"}]}`

	res, err := parseVerifyResult(out, askedThreads())
	if err != nil {
		t.Fatalf("parseVerifyResult: %v", err)
	}

	if len(res.Verdicts) != 1 || res.Verdicts[0].Verdict != VerdictUnfixed {
		t.Fatalf("verdicts = %+v, want only the first answer for T1", res.Verdicts)
	}
}

func TestParseVerifyResultRejectsOutputWithNoJSON(t *testing.T) {
	if _, err := parseVerifyResult("I could not do that", askedThreads()); err == nil {
		t.Fatal("err = nil, want a failure on output carrying no JSON")
	}
}

func TestBuildVerifyPromptCarriesFindingsRepliesAndDiff(t *testing.T) {
	prompt := buildVerifyPrompt(VerifyRequest{
		Repo:     "o/r",
		PRNumber: 111,
		Title:    "Add trace id",
		Diff:     "@@ the delta @@",
		Threads:  askedThreads(),
	})

	for _, want := range []string{
		"T1", "T2",
		"a.ts:30",
		"array bodies skip trace_id",
		"author: fixed",
		"@@ the delta @@",
		"Judge the DIFF, not the reply",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}

	// A thread with no replies must still be presented, or the engine would
	// silently judge fewer threads than were asked about.
	if !strings.Contains(prompt, "replies: (none)") {
		t.Error("a thread with no replies was not rendered")
	}
}

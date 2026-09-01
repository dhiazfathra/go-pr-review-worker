package worker

import (
	"fmt"
	"strings"

	"github.com/dhiazfathra/go-pr-review-worker/internal/reviewer"
)

// summaryMarker tags every comment this worker writes. It makes the worker's
// own comments identifiable in the PR timeline and by future tooling.
const summaryMarker = "<!-- pr-review-worker -->"

var severityBadge = map[reviewer.Severity]string{
	reviewer.SeverityCritical: "🔴 critical",
	reviewer.SeverityMajor:    "🟠 major",
	reviewer.SeverityMinor:    "🟡 minor",
	reviewer.SeverityNit:      "⚪ nit",
}

// renderComment formats one finding as an inline comment.
func renderComment(f reviewer.Finding) string {
	return fmt.Sprintf(
		"%s\n\n**%s** — %s\n\n%s\n",
		summaryMarker,
		severityBadge[f.Severity],
		f.Title,
		strings.TrimSpace(f.Body),
	)
}

// renderSummary formats the per-cycle summary comment.
func renderSummary(res reviewer.Result, cycle, maxCycles int, posted []reviewer.Finding) string {
	var b strings.Builder

	b.WriteString(summaryMarker)
	fmt.Fprintf(&b, "\n\n## Automated review — pass %d of %d\n\n", cycle, maxCycles)

	summary := strings.TrimSpace(res.Summary)
	if summary == "" {
		summary = "_No summary produced._"
	}

	b.WriteString(summary)
	b.WriteString("\n\n")

	switch len(posted) {
	case 0:
		b.WriteString("No new inline comments in this pass.\n")
	default:
		fmt.Fprintf(&b, "### %d inline comment(s)\n\n", len(posted))

		for _, f := range posted {
			fmt.Fprintf(&b, "- %s `%s:%d` — %s\n", severityBadge[f.Severity], f.File, f.Line, f.Title)
		}
	}

	if cycle >= maxCycles {
		fmt.Fprintf(&b, "\n_Final pass. Later pushes to this pull request will not be reviewed._\n")
	}

	engine := res.Engine
	if engine == "" {
		engine = "unknown"
	}

	fmt.Fprintf(&b, "\n<sub>engine: `%s`</sub>\n", engine)

	return b.String()
}

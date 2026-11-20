// Package report turns runs into what ansibled shows on the forge: commit status
// descriptions and the MR comment (DESIGN.md "Reporting").
package report

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/timothywashburn/ansibled/internal/forge"
	"github.com/timothywashburn/ansibled/internal/model"
)

// MaxDescription is the longest commit status description, in runes.
const MaxDescription = 140

// Limits that keep the MR comment bounded.
const (
	maxListItems  = 50   // task entries per <details> list
	maxTableRows  = 100  // hosts per table
	maxNameRunes  = 200  // task and host names
	maxErrorRunes = 1000 // Summary.Error in the error body
)

// Describe returns the commit status description for a run (at most
// MaxDescription runes), e.g. "would change 4 tasks on 2 hosts" or
// "2 failed on db1".
func Describe(r *model.Run) string {
	if r == nil {
		return ""
	}
	return truncate(describe(r), MaxDescription)
}

func describe(r *model.Run) string {
	switch r.State {
	case model.StateQueued:
		return "queued"
	case model.StateRunning:
		return "dry run running"
	case model.StateSuccess:
		changed, _, _, hosts, _ := r.Summary.Totals()
		if changed == 0 {
			return "no changes"
		}
		return fmt.Sprintf("would change %s on %s", plural(changed, "task"), onHosts(hosts, changedHosts(r.Summary)))
	case model.StateFailed:
		if d := describeFailed(r.Summary); d != "" {
			return d
		}
		if r.Description != "" {
			return r.Description
		}
		return "failed"
	case model.StateError:
		if r.Summary != nil && r.Summary.FailedStep != "" {
			return "error: " + stepFailure(r.Summary)
		}
		if r.Description != "" {
			if strings.HasPrefix(r.Description, "error") {
				return r.Description
			}
			return "error: " + r.Description
		}
		return "error"
	case model.StateCanceled:
		if r.Description != "" {
			return r.Description
		}
		return "canceled"
	}
	if r.Description != "" {
		return r.Description
	}
	return string(r.State)
}

// describeFailed mentions failed tasks first, then unreachable hosts.
func describeFailed(s *model.Summary) string {
	if s == nil {
		return ""
	}
	failed := 0
	var failedHosts, unreachable []string
	seenF, seenU := map[string]bool{}, map[string]bool{}
	for _, p := range s.Playbooks {
		for _, h := range p.Hosts {
			if h.Failed > 0 {
				failed += h.Failed
				if !seenF[h.Host] {
					seenF[h.Host] = true
					failedHosts = append(failedHosts, h.Host)
				}
			}
			if h.Unreachable && !seenU[h.Host] {
				seenU[h.Host] = true
				unreachable = append(unreachable, h.Host)
			}
		}
	}
	var parts []string
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed on %s", failed, onHosts(len(failedHosts), failedHosts)))
	}
	if len(unreachable) > 0 {
		if len(unreachable) <= 3 {
			parts = append(parts, "unreachable: "+strings.Join(unreachable, ", "))
		} else {
			parts = append(parts, fmt.Sprintf("unreachable: %d hosts", len(unreachable)))
		}
	}
	return strings.Join(parts, "; ")
}

// changedHosts lists the distinct hosts with changes, in summary order.
func changedHosts(s *model.Summary) []string {
	if s == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, p := range s.Playbooks {
		for _, h := range p.Hosts {
			if h.Changed > 0 && !seen[h.Host] {
				seen[h.Host] = true
				out = append(out, h.Host)
			}
		}
	}
	return out
}

// onHosts names the host when there is exactly one, otherwise counts them.
func onHosts(n int, names []string) string {
	if n == 1 && len(names) == 1 {
		return names[0]
	}
	return plural(n, "host")
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// stepFailure turns Summary.FailedStep into short text such as "setup command failed".
func stepFailure(s *model.Summary) string {
	switch s.FailedStep {
	case "fetch":
		return "fetch failed"
	case "checkout":
		return "checkout failed"
	case "overlay":
		return "overlay failed"
	case "setup":
		return "setup command failed"
	case "collections":
		return "collection install failed"
	case "timeout":
		return "timed out"
	case "ansible":
		for _, p := range s.Playbooks {
			switch p.ExitCode {
			case 0, 2, 4:
			default:
				return fmt.Sprintf("ansible-playbook exited with code %d", p.ExitCode)
			}
		}
		return "ansible-playbook failed"
	}
	return s.FailedStep + " failed"
}

// StatusState maps a run state to a commit status state. error is reported as failed.
func StatusState(s model.State) forge.StatusState {
	switch s {
	case model.StateQueued:
		return forge.StatusPending
	case model.StateRunning:
		return forge.StatusRunning
	case model.StateSuccess:
		return forge.StatusSuccess
	case model.StateFailed, model.StateError:
		return forge.StatusFailed
	case model.StateCanceled:
		return forge.StatusCanceled
	}
	return forge.StatusPending
}

// headings maps a state to its icon and words for the MR comment heading.
var headings = map[model.State][2]string{
	model.StateQueued:   {"⏳", "dry run queued"},
	model.StateRunning:  {"⏳", "dry run running"},
	model.StateSuccess:  {"✅", "dry run passed"},
	model.StateFailed:   {"❌", "dry run failed"},
	model.StateError:    {"⚠️", "dry run error"},
	model.StateCanceled: {"⏹", "dry run canceled"},
}

// MRComment renders the MR comment for a run (DESIGN.md "MR comment"). marker
// is the hidden site marker and comes first; runURL is the run's log page. The
// comment contains only counts and task names, never task messages or diffs.
func MRComment(r *model.Run, site, marker, runURL string) string {
	var b strings.Builder
	h, ok := headings[r.State]
	if !ok {
		h = [2]string{"❔", "dry run " + string(r.State)}
	}
	b.WriteString(marker)
	b.WriteString("\n")
	fmt.Fprintf(&b, "### %s ansibled · %s · %s\n", h[0], Escape(site), h[1])

	logLink := fmt.Sprintf("[full log](%s)", runURL)
	switch r.State {
	case model.StateQueued, model.StateRunning:
		fmt.Fprintf(&b, "⏳ dry run of `%s` %s · %s\n", r.ShortSHA(), r.State, logLink)
		return b.String()
	}

	b.WriteString(shaLine(r, logLink))
	switch r.State {
	case model.StateCanceled:
		if r.Description != "" {
			fmt.Fprintf(&b, "\n⏹ canceled: %s\n", Escape(oneLine(r.Description, maxNameRunes)))
		} else {
			b.WriteString("\n⏹ canceled\n")
		}
		return b.String()
	case model.StateError:
		step := "run"
		if r.Summary != nil && r.Summary.FailedStep != "" {
			step = r.Summary.FailedStep
		}
		fmt.Fprintf(&b, "\n⚠️ error during %s\n", Escape(step))
		if r.Summary != nil && r.Summary.Error != "" {
			fmt.Fprintf(&b, "\n%s\n", codeSpan(oneLine(r.Summary.Error, maxErrorRunes)))
		}
	}
	writeSummary(&b, r.Summary)
	return b.String()
}

// shaLine is "`sha` · duration · [full log](url)".
func shaLine(r *model.Run, logLink string) string {
	parts := []string{"`" + r.ShortSHA() + "`"}
	if d := r.Duration(); d > 0 {
		parts = append(parts, FormatDuration(d))
	}
	parts = append(parts, logLink)
	return strings.Join(parts, " · ") + "\n"
}

// FormatDuration renders a duration rounded to seconds, e.g. "3m12s".
func FormatDuration(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

func writeSummary(b *strings.Builder, s *model.Summary) {
	if s == nil || len(s.Playbooks) == 0 {
		return
	}
	multi := len(s.Playbooks) > 1
	for _, p := range s.Playbooks {
		if multi {
			fmt.Fprintf(b, "\n#### %s\n", Escape(oneLine(p.Playbook, maxNameRunes)))
		}
		writeTable(b, p.Hosts)
		writeTaskList(b, "Would change", p.Hosts, func(h model.HostResult) []string { return h.ChangedTasks })
		writeTaskList(b, "Failed", p.Hosts, func(h model.HostResult) []string { return h.FailedTasks })
	}
}

func writeTable(b *strings.Builder, hosts []model.HostResult) {
	if len(hosts) == 0 {
		b.WriteString("\nno hosts matched\n")
		return
	}
	b.WriteString("\n| host | would change | failed | unreachable |\n|---|---:|---:|:-:|\n")
	for i, h := range hosts {
		if i == maxTableRows {
			fmt.Fprintf(b, "| … and %d more | | | |\n", len(hosts)-i)
			break
		}
		unreach := ""
		if h.Unreachable {
			unreach = "yes"
		}
		fmt.Fprintf(b, "| %s | %d | %d | %s |\n", Escape(oneLine(h.Host, maxNameRunes)), h.Changed, h.Failed, unreach)
	}
}

func writeTaskList(b *strings.Builder, title string, hosts []model.HostResult, tasks func(model.HostResult) []string) {
	total := 0
	for _, h := range hosts {
		total += len(tasks(h))
	}
	if total == 0 {
		return
	}
	fmt.Fprintf(b, "\n<details><summary>%s (%d)</summary>\n\n", title, total)
	n := 0
	for _, h := range hosts {
		host := Escape(oneLine(h.Host, maxNameRunes))
		for _, t := range tasks(h) {
			if n == maxListItems {
				fmt.Fprintf(b, "- … and %d more\n", total-n)
				b.WriteString("\n</details>\n")
				return
			}
			fmt.Fprintf(b, "- %s: %s\n", host, Escape(oneLine(t, maxNameRunes)))
			n++
		}
	}
	b.WriteString("\n</details>\n")
}

// mdEscaper backslash-escapes characters that markdown (and GitLab's
// references and mentions) would otherwise interpret.
var mdEscaper = strings.NewReplacer(
	`\`, `\\`, "|", `\|`, "`", "\\`", "*", `\*`, "_", `\_`,
	"<", `\<`, ">", `\>`, "[", `\[`, "]", `\]`, "~", `\~`,
	"#", `\#`, "@", `\@`, "&", `\&`, "$", `\$`, "!", `\!`,
)

// Escape makes s safe to embed as plain text in GitLab-flavoured markdown,
// including table cells.
func Escape(s string) string { return mdEscaper.Replace(s) }

// oneLine collapses whitespace runs (including newlines) to single spaces and
// truncates to max runes.
func oneLine(s string, max int) string {
	return truncate(strings.Join(strings.Fields(s), " "), max)
}

// truncate shortens s to at most max runes, ending in "…" when cut.
func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}

// codeSpan wraps s in a markdown code span whose fence is longer than any
// backtick run inside s.
func codeSpan(s string) string {
	longest, run := 0, 0
	for _, c := range s {
		if c == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	if longest > 0 || strings.HasPrefix(s, " ") || strings.HasSuffix(s, " ") {
		return fence + " " + s + " " + fence
	}
	return fence + s + fence
}

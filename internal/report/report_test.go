package report

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/timothywashburn/ansibled/internal/forge"
	"github.com/timothywashburn/ansibled/internal/model"
)

const marker = "<!-- ansibled:site=lab -->"

func run(state model.State, s *model.Summary) *model.Run {
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return &model.Run{
		ID: 412, Kind: model.KindMR, SHA: "a1b2c3d4e5f60718293a", State: state, Summary: s,
		StartedAt: start, FinishedAt: start.Add(3*time.Minute + 12*time.Second),
	}
}

func pb(name string, hosts ...model.HostResult) model.PlaybookResult {
	return model.PlaybookResult{Playbook: name, Hosts: hosts}
}

func TestDescribe(t *testing.T) {
	tests := []struct {
		name string
		run  *model.Run
		want string
	}{
		{"queued", run(model.StateQueued, nil), "queued"},
		{"running", run(model.StateRunning, nil), "dry run running"},
		{"no changes", run(model.StateSuccess, &model.Summary{Playbooks: []model.PlaybookResult{pb("site.yml", model.HostResult{Host: "a", OK: 3})}}), "no changes"},
		{"nil summary success", run(model.StateSuccess, nil), "no changes"},
		{"changes on hosts", run(model.StateSuccess, &model.Summary{Playbooks: []model.PlaybookResult{
			pb("site.yml", model.HostResult{Host: "a", Changed: 3}, model.HostResult{Host: "b", Changed: 1}),
		}}), "would change 4 tasks on 2 hosts"},
		{"change on one host", run(model.StateSuccess, &model.Summary{Playbooks: []model.PlaybookResult{
			pb("site.yml", model.HostResult{Host: "a", Changed: 1}, model.HostResult{Host: "b"}),
		}}), "would change 1 task on a"},
		{"failed one host", run(model.StateFailed, &model.Summary{Playbooks: []model.PlaybookResult{
			pb("site.yml", model.HostResult{Host: "db1", Failed: 2}),
		}}), "2 failed on db1"},
		{"failed two hosts across playbooks", run(model.StateFailed, &model.Summary{Playbooks: []model.PlaybookResult{
			pb("a.yml", model.HostResult{Host: "x", Failed: 1}),
			pb("b.yml", model.HostResult{Host: "x", Failed: 1}, model.HostResult{Host: "y", Failed: 1}),
		}}), "3 failed on 2 hosts"},
		{"unreachable", run(model.StateFailed, &model.Summary{Playbooks: []model.PlaybookResult{
			pb("site.yml", model.HostResult{Host: "web3", Unreachable: true}),
		}}), "unreachable: web3"},
		{"failed and unreachable", run(model.StateFailed, &model.Summary{Playbooks: []model.PlaybookResult{
			pb("site.yml", model.HostResult{Host: "a", Failed: 1}, model.HostResult{Host: "b", Unreachable: true}),
		}}), "1 failed on a; unreachable: b"},
		{"many unreachable", run(model.StateFailed, &model.Summary{Playbooks: []model.PlaybookResult{
			pb("site.yml", model.HostResult{Host: "a", Unreachable: true}, model.HostResult{Host: "b", Unreachable: true},
				model.HostResult{Host: "c", Unreachable: true}, model.HostResult{Host: "d", Unreachable: true}),
		}}), "unreachable: 4 hosts"},
		{"error setup", run(model.StateError, &model.Summary{FailedStep: "setup"}), "error: setup command failed"},
		{"error timeout", run(model.StateError, &model.Summary{FailedStep: "timeout"}), "error: timed out"},
		{"error ansible", run(model.StateError, &model.Summary{FailedStep: "ansible", Playbooks: []model.PlaybookResult{{Playbook: "a", ExitCode: 2}, {Playbook: "b", ExitCode: 250}}}), "error: ansible-playbook exited with code 250"},
		{"error unknown", run(model.StateError, nil), "error"},
		{"canceled", run(model.StateCanceled, nil), "canceled"},
		{"canceled desc", func() *model.Run {
			r := run(model.StateCanceled, nil)
			r.Description = "superseded by a1b2c3d"
			return r
		}(), "superseded by a1b2c3d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Describe(tt.run); got != tt.want {
				t.Errorf("Describe = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDescribeTruncates(t *testing.T) {
	r := run(model.StateCanceled, nil)
	r.Description = strings.Repeat("é", 300)
	got := Describe(r)
	if n := utf8.RuneCountInString(got); n != MaxDescription {
		t.Fatalf("len = %d runes", n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("no ellipsis: %q", got)
	}
}

func TestStatusState(t *testing.T) {
	want := map[model.State]forge.StatusState{
		model.StateQueued:   forge.StatusPending,
		model.StateRunning:  forge.StatusRunning,
		model.StateSuccess:  forge.StatusSuccess,
		model.StateFailed:   forge.StatusFailed,
		model.StateError:    forge.StatusFailed,
		model.StateCanceled: forge.StatusCanceled,
	}
	for s, w := range want {
		if got := StatusState(s); got != w {
			t.Errorf("StatusState(%s) = %s, want %s", s, got, w)
		}
	}
}

func TestMRCommentSuccess(t *testing.T) {
	r := run(model.StateSuccess, &model.Summary{Playbooks: []model.PlaybookResult{pb("site.yml",
		model.HostResult{Host: "web1", Changed: 3, ChangedTasks: []string{"nginx : deploy site config", "a", "b"}},
		model.HostResult{Host: "db1", Changed: 1, ChangedTasks: []string{"c"}},
	)}})
	got := MRComment(r, "lab", marker, "http://ansibled.example/runs/412")
	want := marker + `
### ✅ ansibled · lab · dry run passed
` + "`a1b2c3d4`" + ` · 3m12s · [full log](http://ansibled.example/runs/412)

| host | would change | failed | unreachable |
|---|---:|---:|:-:|
| web1 | 3 | 0 |  |
| db1 | 1 | 0 |  |

<details><summary>Would change (4)</summary>

- web1: nginx : deploy site config
- web1: a
- web1: b
- db1: c

</details>
`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestMRCommentFailedMultiPlaybook(t *testing.T) {
	r := run(model.StateFailed, &model.Summary{Playbooks: []model.PlaybookResult{
		pb("one.yml", model.HostResult{Host: "a", Failed: 1, FailedTasks: []string{"boom"}}),
		pb("two.yml", model.HostResult{Host: "b", Unreachable: true}),
	}})
	got := MRComment(r, "lab", marker, "u")
	for _, s := range []string{"### ❌ ansibled · lab · dry run failed", "#### one.yml", "#### two.yml",
		"<details><summary>Failed (1)</summary>", "- a: boom", "| b | 0 | 0 | yes |"} {
		if !strings.Contains(got, s) {
			t.Errorf("missing %q in:\n%s", s, got)
		}
	}
	if strings.Contains(got, "Would change") {
		t.Errorf("empty would-change list rendered:\n%s", got)
	}
}

func TestMRCommentEscapes(t *testing.T) {
	evil := "x | y `z` *b* _i_ <img src=x> [l](http://e) @all #1\nsecond line"
	r := run(model.StateSuccess, &model.Summary{Playbooks: []model.PlaybookResult{pb("site.yml",
		model.HostResult{Host: "h|<b>", Changed: 1, ChangedTasks: []string{evil}},
	)}})
	got := MRComment(r, "lab", marker, "u")
	want := `- h\|\<b\>: x \| y \` + "`z\\`" + ` \*b\* \_i\_ \<img src=x\> \[l\](http://e) \@all \#1 second line`
	if !strings.Contains(got, want) {
		t.Errorf("missing %q in:\n%s", want, got)
	}
	if stripped := strings.NewReplacer(`\<`, "", `\@`, "").Replace(got); strings.Contains(stripped, "<img") || strings.Contains(stripped, "@all") {
		t.Errorf("unescaped content:\n%s", got)
	}
}

func TestMRCommentTruncatesLists(t *testing.T) {
	var tasks []string
	for i := range 120 {
		tasks = append(tasks, fmt.Sprintf("task %d", i))
	}
	r := run(model.StateSuccess, &model.Summary{Playbooks: []model.PlaybookResult{pb("site.yml",
		model.HostResult{Host: "a", Changed: 120, ChangedTasks: tasks},
	)}})
	got := MRComment(r, "lab", marker, "u")
	if !strings.Contains(got, "Would change (120)") || !strings.Contains(got, "- … and 70 more") {
		t.Errorf("not truncated:\n%s", got)
	}
	if strings.Contains(got, "task 50") {
		t.Errorf("too many items")
	}
}

func TestMRCommentShortStates(t *testing.T) {
	q := run(model.StateQueued, nil)
	got := MRComment(q, "lab", marker, "u")
	if !strings.HasPrefix(got, marker+"\n") || !strings.Contains(got, "⏳ dry run of `a1b2c3d4` queued") {
		t.Errorf("queued:\n%s", got)
	}
	ru := run(model.StateRunning, nil)
	if got := MRComment(ru, "lab", marker, "u"); !strings.Contains(got, "⏳ dry run of `a1b2c3d4` running") {
		t.Errorf("running:\n%s", got)
	}

	c := run(model.StateCanceled, nil)
	c.Description = "superseded by 0badc0de"
	if got := MRComment(c, "lab", marker, "u"); !strings.Contains(got, "⏹ canceled: superseded by 0badc0de") {
		t.Errorf("canceled:\n%s", got)
	}

	e := run(model.StateError, &model.Summary{FailedStep: "setup", Error: "exit status 1: `uv` not found"})
	got = MRComment(e, "lab", marker, "u")
	if !strings.Contains(got, "⚠️ error during setup") || !strings.Contains(got, "`` exit status 1: `uv` not found ``") {
		t.Errorf("error:\n%s", got)
	}
}

func TestCodeSpan(t *testing.T) {
	for in, want := range map[string]string{
		"plain":    "`plain`",
		"a ` b":    "`` a ` b ``",
		"a `` b":   "``` a `` b ```",
		" padded ": "`  padded  `",
	} {
		if got := codeSpan(in); got != want {
			t.Errorf("codeSpan(%q) = %q, want %q", in, got, want)
		}
	}
}

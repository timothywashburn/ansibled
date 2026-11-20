package engine

import (
	"reflect"
	"strings"
	"testing"

	"github.com/timothywashburn/ansibled/internal/model"
)

func lines(ls ...string) string { return strings.Join(ls, "\n") + "\n" }

func TestParseEvents(t *testing.T) {
	tests := []struct {
		name   string
		events string
		exit   int
		want   []model.HostResult
	}{
		{
			name: "success with changes",
			events: lines(
				`{"event":"play_start","playbook":"site.yml","play":"all"}`,
				`{"event":"task_start","playbook":"site.yml","play":"all","task":"ping","path":"site.yml:3"}`,
				`{"event":"result","playbook":"site.yml","status":"ok","host":"web2","task":"ping"}`,
				`{"event":"result","playbook":"site.yml","status":"ok","host":"web1","task":"ping"}`,
				`{"event":"result","playbook":"site.yml","status":"changed","host":"web1","task":"docker : config"}`,
				`{"event":"result","playbook":"site.yml","status":"changed","host":"web1","task":"docker : restart"}`,
				`{"event":"result","playbook":"site.yml","status":"skipped","host":"web2","task":"docker : config"}`,
				`{"event":"stats","playbook":"site.yml","hosts":{"web1":{"ok":3,"changed":2,"failures":0,"unreachable":0,"skipped":0,"rescued":0,"ignored":0},"web2":{"ok":1,"changed":0,"failures":0,"unreachable":0,"skipped":1,"rescued":0,"ignored":0}}}`,
			),
			want: []model.HostResult{
				{Host: "web1", OK: 3, Changed: 2, ChangedTasks: []string{"docker : config", "docker : restart"}},
				{Host: "web2", OK: 1, Skipped: 1},
			},
		},
		{
			name: "failures and ignored errors",
			exit: 2,
			events: lines(
				`{"event":"result","status":"failed","host":"a","task":"tolerated","ignore_errors":true,"msg":"x"}`,
				`{"event":"result","status":"failed","host":"a","task":"broken","ignore_errors":false,"msg":"boom"}`,
				`{"event":"result","status":"changed","host":"b","task":"tpl"}`,
				`{"event":"stats","hosts":{"a":{"ok":1,"changed":0,"failures":1,"unreachable":0,"skipped":0,"rescued":0,"ignored":1},"b":{"ok":1,"changed":1,"failures":0,"unreachable":0,"skipped":0,"rescued":0,"ignored":0}}}`,
			),
			want: []model.HostResult{
				{Host: "a", OK: 1, Failed: 1, Ignored: 1, FailedTasks: []string{"broken"}},
				{Host: "b", OK: 1, Changed: 1, ChangedTasks: []string{"tpl"}},
			},
		},
		{
			name: "unreachable",
			exit: 4,
			events: lines(
				`{"event":"result","status":"unreachable","host":"gone","task":"ping","msg":"ssh: connect to host"}`,
				`{"event":"result","status":"ok","host":"here","task":"ping"}`,
				`{"event":"stats","hosts":{"gone":{"ok":0,"changed":0,"failures":0,"unreachable":1,"skipped":0,"rescued":0,"ignored":0},"here":{"ok":1,"changed":0,"failures":0,"unreachable":0,"skipped":0,"rescued":0,"ignored":0}}}`,
			),
			want: []model.HostResult{
				{Host: "gone", Unreachable: true},
				{Host: "here", OK: 1},
			},
		},
		{
			name: "rescued failure is not listed",
			events: lines(
				`{"event":"result","status":"failed","host":"a","task":"try"}`,
				`{"event":"result","status":"ok","host":"a","task":"rescue"}`,
				`{"event":"stats","hosts":{"a":{"ok":1,"changed":0,"failures":0,"unreachable":0,"skipped":0,"rescued":1,"ignored":0}}}`,
			),
			want: []model.HostResult{{Host: "a", OK: 1, Rescued: 1}},
		},
		{
			name: "missing stats falls back to counting, tolerates junk",
			exit: -1,
			events: lines(
				`{"event":"play_start","play":"all"}`,
				`not json`,
				`{"event":"result","status":"ok","host":"b","task":"ping"}`,
				`{"event":"result","status":"changed","host":"b","task":"tpl"}`,
				`{"event":"result","status":"failed","host":"b","task":"ign","ignore_errors":true}`,
				`{"event":"result","status":"failed","host":"a","task":"bad"}`,
				`{"event":"result","status":"skipped","host":"a","task":"maybe"}`,
				`{"event":"result","status":"unreachable","host":"c","task":"ping"}`,
				`{"event":"result","status":"ok","host":"","task":"no host"}`,
			) + `{"event":"result","status":"changed","host":"a","ta`, // truncated by a kill
			want: []model.HostResult{
				{Host: "a", Failed: 1, Skipped: 1, FailedTasks: []string{"bad"}},
				{Host: "b", OK: 3, Changed: 1, Ignored: 1, ChangedTasks: []string{"tpl"}},
				{Host: "c", Unreachable: true},
			},
		},
		{
			name:   "empty",
			events: "",
			want:   []model.HostResult{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseEvents(strings.NewReader(tc.events), "site.yml", tc.exit)
			if err != nil {
				t.Fatal(err)
			}
			if got.Playbook != "site.yml" || got.ExitCode != tc.exit {
				t.Errorf("playbook/exit = %q/%d", got.Playbook, got.ExitCode)
			}
			if !reflect.DeepEqual(got.Hosts, tc.want) {
				t.Errorf("hosts:\n got  %+v\n want %+v", got.Hosts, tc.want)
			}
		})
	}
}

func TestPlaybookState(t *testing.T) {
	ok := model.PlaybookResult{Playbook: "a.yml", Hosts: []model.HostResult{{Host: "h", Changed: 3}}}
	failed := model.PlaybookResult{Playbook: "b.yml", ExitCode: 2, Hosts: []model.HostResult{{Host: "h", Failed: 1}}}
	unreach := model.PlaybookResult{Playbook: "c.yml", ExitCode: 4, Hosts: []model.HostResult{{Host: "h", Unreachable: true}}}
	broken := model.PlaybookResult{Playbook: "d.yml", ExitCode: 250}
	tests := []struct {
		name      string
		pbs       []model.PlaybookResult
		want      model.State
		wantError string
	}{
		{"all ok, changes allowed", []model.PlaybookResult{ok, ok}, model.StateSuccess, ""},
		{"failure", []model.PlaybookResult{ok, failed}, model.StateFailed, ""},
		{"unreachable", []model.PlaybookResult{unreach}, model.StateFailed, ""},
		{"exit 2 without parsed failures", []model.PlaybookResult{{Playbook: "x.yml", ExitCode: 2}}, model.StateFailed, ""},
		{"error wins", []model.PlaybookResult{failed, broken, ok}, model.StateError, "d.yml exited 250"},
		{"error first", []model.PlaybookResult{broken, failed}, model.StateError, "d.yml exited 250"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sum := &model.Summary{Playbooks: tc.pbs}
			if got := playbookState(sum); got != tc.want {
				t.Errorf("state = %s, want %s", got, tc.want)
			}
			if sum.Error != tc.wantError {
				t.Errorf("error = %q, want %q", sum.Error, tc.wantError)
			}
			if tc.wantError != "" && sum.FailedStep != "ansible" {
				t.Errorf("failed step = %q", sum.FailedStep)
			}
		})
	}
}

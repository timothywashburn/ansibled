package store

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/timothywashburn/ansibled/internal/model"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "ansibled.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func create(t *testing.T, s *Store, r *model.Run) *model.Run {
	t.Helper()
	if err := s.CreateRun(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	return r
}

func ids(rs []*model.Run) []int64 {
	out := []int64{}
	for _, r := range rs {
		out = append(out, r.ID)
	}
	return out
}

func TestCreateDefaultsAndRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	r := create(t, s, &model.Run{Kind: model.KindMR, MRIID: 7, SHA: "abc", Ref: "feature", Trigger: model.TriggerPush, TriggeredBy: "alice", State: model.StateQueued})
	if r.ID == 0 || r.Key != "mr:7" || r.Mode != model.ModeCheck || r.QueuedAt.IsZero() {
		t.Fatalf("defaults not applied: %+v", r)
	}

	got, err := s.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.StartedAt.IsZero() || !got.FinishedAt.IsZero() || got.Summary != nil {
		t.Fatalf("zero values did not round-trip: %+v", got)
	}
	if !got.QueuedAt.Equal(r.QueuedAt) {
		t.Fatalf("queued_at %v != %v", got.QueuedAt, r.QueuedAt)
	}

	// Full update, including summary and times.
	r.State = model.StateFailed
	r.Description = "2 failed on host1"
	r.StartedAt = time.Now().Add(-time.Minute)
	r.FinishedAt = time.Now()
	r.LogsExpired = true
	r.Summary = &model.Summary{Playbooks: []model.PlaybookResult{{
		Playbook: "site.yml", ExitCode: 2,
		Hosts: []model.HostResult{{Host: "host1", OK: 3, Changed: 1, Failed: 2, ChangedTasks: []string{"a"}, FailedTasks: []string{"b", "c"}}},
	}}}
	if err := s.UpdateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.StartedAt.Equal(r.StartedAt) || !got.FinishedAt.Equal(r.FinishedAt) || !got.QueuedAt.Equal(r.QueuedAt) {
		t.Fatalf("times: %+v vs %+v", got, r)
	}
	// Compare the rest field by field via normalised copies.
	want := *r
	want.QueuedAt, want.StartedAt, want.FinishedAt = got.QueuedAt, got.StartedAt, got.FinishedAt
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", *got, want)
	}

	if err := s.UpdateRun(ctx, &model.Run{ID: 999}); err == nil {
		t.Fatal("update of missing run should fail")
	}
	if g, err := s.GetRun(ctx, 999); g != nil || err != nil {
		t.Fatalf("missing run: %v %v", g, err)
	}
}

func TestQueries(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()

	r1 := create(t, s, &model.Run{Kind: model.KindMain, SHA: "s1", State: model.StateSuccess, FinishedAt: now.Add(-3 * time.Hour)})
	r2 := create(t, s, &model.Run{Kind: model.KindMR, MRIID: 1, SHA: "s2", State: model.StateCanceled, FinishedAt: now})
	r3 := create(t, s, &model.Run{Kind: model.KindMR, MRIID: 1, SHA: "s3", State: model.StateQueued})
	r4 := create(t, s, &model.Run{Kind: model.KindMR, MRIID: 1, SHA: "s4", State: model.StateRunning})
	r5 := create(t, s, &model.Run{Kind: model.KindNightly, SHA: "s1", State: model.StateFailed, FinishedAt: now.Add(-time.Hour)})
	r6 := create(t, s, &model.Run{Kind: model.KindMain, SHA: "s5", State: model.StateQueued})

	runs, err := s.ListRuns(ctx, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(runs); !reflect.DeepEqual(got, []int64{r5.ID, r4.ID}) {
		t.Fatalf("ListRuns = %v", got)
	}
	runs, _ = s.ListRuns(ctx, 0, 0)
	if len(runs) != 6 {
		t.Fatalf("ListRuns unlimited = %d", len(runs))
	}

	active, err := s.ActiveByKey(ctx, "mr:1")
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(active); !reflect.DeepEqual(got, []int64{r3.ID, r4.ID}) {
		t.Fatalf("ActiveByKey = %v", got)
	}

	for _, c := range []struct {
		key, sha string
		want     bool
	}{
		{"main", "s1", true},
		{"mr:1", "s2", false}, // canceled does not count
		{"mr:1", "s3", true},
		{"mr:1", "s4", true},
		{"nightly", "s1", true},
		{"mr:2", "s3", false},
	} {
		got, err := s.Exists(ctx, c.key, c.sha)
		if err != nil || got != c.want {
			t.Errorf("Exists(%s, %s) = %v, %v; want %v", c.key, c.sha, got, err, c.want)
		}
	}

	next, err := s.NextQueued(ctx)
	if err != nil || next == nil || next.ID != r3.ID {
		t.Fatalf("NextQueued = %+v, %v", next, err)
	}

	byState, _ := s.ListByState(ctx, model.StateQueued, model.StateRunning)
	if got := ids(byState); !reflect.DeepEqual(got, []int64{r3.ID, r4.ID, r6.ID}) {
		t.Fatalf("ListByState = %v", got)
	}
	if n, _ := s.CountByState(ctx, model.StateQueued); n != 2 {
		t.Fatalf("CountByState(queued) = %d", n)
	}
	if n, _ := s.CountByState(ctx); n != 0 {
		t.Fatalf("CountByState() = %d", n)
	}

	latest, err := s.LatestFinished(ctx, model.KindMain, model.KindNightly)
	if err != nil || latest == nil || latest.ID != r5.ID {
		t.Fatalf("LatestFinished(main,nightly) = %+v, %v", latest, err)
	}
	latest, _ = s.LatestFinished(ctx, model.KindMain)
	if latest == nil || latest.ID != r1.ID {
		t.Fatalf("LatestFinished(main) = %+v", latest)
	}
	latest, _ = s.LatestFinished(ctx) // any kind; r2 is canceled so r5 wins
	if latest == nil || latest.ID != r5.ID {
		t.Fatalf("LatestFinished() = %+v", latest)
	}
	if latest, _ = s.LatestFinished(ctx, model.KindMR); latest != nil {
		t.Fatalf("LatestFinished(mr) = %+v", latest)
	}

	old, err := s.LogsOlderThan(ctx, now.Add(-30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(old, []int64{r1.ID, r5.ID}) {
		t.Fatalf("LogsOlderThan = %v", old)
	}
	if err := s.MarkLogsExpired(ctx, r1.ID); err != nil {
		t.Fatal(err)
	}
	old, _ = s.LogsOlderThan(ctx, now.Add(time.Minute))
	if !reflect.DeepEqual(old, []int64{r2.ID, r5.ID}) {
		t.Fatalf("LogsOlderThan after expiry = %v", old)
	}
	if g, _ := s.GetRun(ctx, r1.ID); !g.LogsExpired {
		t.Fatal("logs_expired not set")
	}
}

func TestEmptyStore(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if r, err := s.NextQueued(ctx); r != nil || err != nil {
		t.Fatalf("NextQueued = %v, %v", r, err)
	}
	if r, err := s.LatestFinished(ctx); r != nil || err != nil {
		t.Fatalf("LatestFinished = %v, %v", r, err)
	}
}

func TestReopenKeepsDataAndVersion(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ansibled.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	r := create(t, s, &model.Run{Kind: model.KindMain, SHA: "x", State: model.StateQueued})
	s.Close()

	s, err = Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != len(migrations) {
		t.Fatalf("user_version = %d, %v", v, err)
	}
	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v", mode, err)
	}
	if g, _ := s.GetRun(context.Background(), r.ID); g == nil || g.SHA != "x" {
		t.Fatalf("run lost after reopen: %+v", g)
	}
}

func TestConcurrentWrites(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	errs := make(chan error, 20)
	for range 20 {
		go func() {
			r := &model.Run{Kind: model.KindMain, SHA: "c", State: model.StateQueued}
			if err := s.CreateRun(ctx, r); err != nil {
				errs <- err
				return
			}
			r.State = model.StateSuccess
			errs <- s.UpdateRun(ctx, r)
		}()
	}
	for range 20 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := s.CountByState(ctx, model.StateSuccess); n != 20 {
		t.Fatalf("count = %d", n)
	}
}

func TestFilterRuns(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	mk := func(kind model.Kind, mr int, st model.State) *model.Run {
		return create(t, s, &model.Run{Kind: kind, Key: model.Key(kind, mr), Mode: model.ModeCheck, SHA: "a", MRIID: mr, State: st})
	}
	a := mk(model.KindMR, 3, model.StateSuccess)
	b := mk(model.KindMain, 0, model.StateFailed)
	c := mk(model.KindMR, 4, model.StateFailed)
	d := mk(model.KindNightly, 0, model.StateSuccess)
	for name, tc := range map[string]struct {
		f    model.RunFilter
		want []int64
	}{
		"all":     {model.RunFilter{}, []int64{d.ID, c.ID, b.ID, a.ID}},
		"kind":    {model.RunFilter{Kinds: []model.Kind{model.KindMR}}, []int64{c.ID, a.ID}},
		"kinds":   {model.RunFilter{Kinds: []model.Kind{model.KindMain, model.KindNightly}}, []int64{d.ID, b.ID}},
		"state":   {model.RunFilter{States: []model.State{model.StateFailed}}, []int64{c.ID, b.ID}},
		"mr":      {model.RunFilter{MRIID: 3}, []int64{a.ID}},
		"combine": {model.RunFilter{Kinds: []model.Kind{model.KindMR}, States: []model.State{model.StateFailed}}, []int64{c.ID}},
	} {
		got, err := s.FilterRuns(ctx, tc.f, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(ids(got), tc.want) {
			t.Errorf("%s: %v, want %v", name, ids(got), tc.want)
		}
	}
	if got, _ := s.FilterRuns(ctx, model.RunFilter{}, 2, 1); !reflect.DeepEqual(ids(got), []int64{c.ID, b.ID}) {
		t.Errorf("limit/offset: %v", ids(got))
	}
}

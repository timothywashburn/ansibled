package gitlab

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timothywashburn/ansibled/internal/forge"
	"github.com/timothywashburn/ansibled/internal/forge/gitlab/gitlabtest"
)

const testSecret = "webhook-s3cret"

func hookClient(t *testing.T) *Client {
	t.Helper()
	c, err := New(Options{
		BaseURL: "https://gitlab.example.org", Project: "myorg/infra", Token: "tok",
		WebhookSecret: testSecret, TrustedGroup: "myorg/sysadmins", Logger: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// HandleWebhook must never call the API.
	c.http = &http.Client{Transport: failTransport{t}}
	return c
}

type failTransport struct{ t *testing.T }

func (f failTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.t.Errorf("unexpected API call during webhook handling: %s %s", r.Method, r.URL)
	return nil, errors.New("no network")
}

func fixtureRequest(t *testing.T, file, event, secret string) *http.Request {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	return gitlabtest.WebhookRequest(event, secret, b)
}

// one runs HandleWebhook and expects exactly one event.
func one(t *testing.T, c *Client, r *http.Request) forge.Event {
	t.Helper()
	evs, err := c.HandleWebhook(r)
	if err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(evs), evs)
	}
	return evs[0]
}

func TestWebhookFixtures(t *testing.T) {
	const (
		head = "9f2c1e5b7d3a4c8e0f1a2b3c4d5e6f708192a3b4"
		url7 = "https://gitlab.example.org/myorg/infra/-/merge_requests/7"
	)
	tests := []struct {
		file, event string
		want        forge.Event
	}{
		{"push.json", "Push Hook", forge.Event{
			Type: forge.EventPush, Name: "Push Hook", Branch: "feature/vlan-42", SHA: head,
			Pusher: forge.User{ID: 42, Username: "alice"},
		}},
		{"push_delete.json", "Push Hook", forge.Event{Type: forge.EventIgnored, Name: "Push Hook"}},
		{"tag_push.json", "Tag Push Hook", forge.Event{Type: forge.EventIgnored, Name: "Tag Push Hook"}},
		// A tag payload under the Push Hook header is still not a branch push.
		{"tag_push.json", "Push Hook", forge.Event{Type: forge.EventIgnored, Name: "Push Hook"}},
		{"mr_open.json", "Merge Request Hook", forge.Event{
			Type: forge.EventMR, Name: "Merge Request Hook", MRAction: forge.MROpen,
			MR: forge.MR{IID: 7, SourceBranch: "feature/vlan-42", HeadSHA: head, State: "opened", WebURL: url7},
		}},
		{"mr_merge.json", "Merge Request Hook", forge.Event{
			Type: forge.EventMR, Name: "Merge Request Hook", MRAction: forge.MRMerge,
			MR: forge.MR{IID: 7, SourceBranch: "feature/vlan-42", HeadSHA: head, State: "merged", WebURL: url7},
		}},
		{"mr_fork_reopen.json", "Merge Request Hook", forge.Event{
			Type: forge.EventMR, Name: "Merge Request Hook", MRAction: forge.MRReopen,
			MR: forge.MR{IID: 7, SourceBranch: "feature/vlan-42", HeadSHA: head, FromFork: true, State: "opened", WebURL: url7},
		}},
		{"mr_update.json", "Merge Request Hook", forge.Event{Type: forge.EventIgnored, Name: "Merge Request Hook"}},
		{"mr_approved.json", "Merge Request Hook", forge.Event{Type: forge.EventIgnored, Name: "Merge Request Hook"}},
		{"note_mr.json", "Note Hook", forge.Event{
			Type: forge.EventComment, Name: "Note Hook", MRIID: 7,
			Author: forge.User{ID: 42, Username: "alice"}, Body: "  /ansibled check\nplease",
			MR: forge.MR{IID: 7, SourceBranch: "feature/vlan-42", HeadSHA: head, State: "opened", WebURL: url7},
		}},
		{"note_mr.json", "Confidential Note Hook", forge.Event{
			Type: forge.EventComment, Name: "Confidential Note Hook", MRIID: 7,
			Author: forge.User{ID: 42, Username: "alice"}, Body: "  /ansibled check\nplease",
			MR: forge.MR{IID: 7, SourceBranch: "feature/vlan-42", HeadSHA: head, State: "opened", WebURL: url7},
		}},
		{"note_commit.json", "Note Hook", forge.Event{Type: forge.EventIgnored, Name: "Note Hook"}},
		{"note_issue.json", "Note Hook", forge.Event{Type: forge.EventIgnored, Name: "Note Hook"}},
		{"push.json", "Pipeline Hook", forge.Event{Type: forge.EventIgnored, Name: "Pipeline Hook"}},
		{"push.json", "", forge.Event{Type: forge.EventIgnored, Name: ""}},
	}
	c := hookClient(t)
	for _, tt := range tests {
		t.Run(tt.file+"/"+tt.event, func(t *testing.T) {
			got := one(t, c, fixtureRequest(t, tt.file, tt.event, testSecret))
			if got != tt.want {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestWebhookAuth(t *testing.T) {
	c := hookClient(t)
	for name, secret := range map[string]string{
		"missing": "", "wrong": "webhook-s3creT", "prefix": "webhook", "longer": testSecret + "x",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := c.HandleWebhook(fixtureRequest(t, "push.json", "Push Hook", secret))
			if !errors.Is(err, forge.ErrUnauthorized) {
				t.Fatalf("err = %v, want ErrUnauthorized", err)
			}
		})
	}
}

func TestWebhookBodyLimit(t *testing.T) {
	c := hookClient(t)
	// Exactly at the limit is fine as far as size goes (but it's not JSON).
	big := bytes.Repeat([]byte(" "), MaxWebhookBody)
	if _, err := c.HandleWebhook(gitlabtest.WebhookRequest("Pipeline Hook", testSecret, big)); err != nil {
		t.Errorf("at limit: %v", err)
	}
	_, err := c.HandleWebhook(gitlabtest.WebhookRequest("Push Hook", testSecret, append(big, ' ')))
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("over limit: err = %v", err)
	}
}

func TestWebhookMalformed(t *testing.T) {
	c := hookClient(t)
	for _, tc := range []struct{ event, body string }{
		{"Push Hook", "{not json"},
		{"Push Hook", `{"object_kind":"push","after":"abc"}`},           // no ref
		{"Push Hook", `{"object_kind":"push","ref":"refs/heads/main"}`}, // no after
		{"Merge Request Hook", `{"object_kind":"merge_request"}`},
		{"Note Hook", `{"object_kind":"note","object_attributes":{"noteable_type":"MergeRequest","note":"hi"}}`},
	} {
		if _, err := c.HandleWebhook(gitlabtest.WebhookRequest(tc.event, testSecret, []byte(tc.body))); err == nil || errors.Is(err, forge.ErrUnauthorized) {
			t.Errorf("%s %s: err = %v, want parse error", tc.event, tc.body, err)
		}
	}
	// Branch deletion with a SHA-256 zero id is also ignored.
	body := `{"object_kind":"push","ref":"refs/heads/x","after":"` + strings.Repeat("0", 64) + `"}`
	ev := one(t, c, gitlabtest.WebhookRequest("Push Hook", testSecret, []byte(body)))
	if ev.Type != forge.EventIgnored {
		t.Errorf("sha256 deletion: %v", ev.Type)
	}
	// System notes are ignored.
	body = `{"object_kind":"note","user":{"id":1},"object_attributes":{"noteable_type":"MergeRequest","note":"added 1 commit","system":true},"merge_request":{"iid":3}}`
	ev = one(t, c, gitlabtest.WebhookRequest("Note Hook", testSecret, []byte(body)))
	if ev.Type != forge.EventIgnored {
		t.Errorf("system note: %v", ev.Type)
	}
}

// The gitlabtest helpers produce requests the adapter understands.
func TestWebhookHelpersRoundTrip(t *testing.T) {
	c := hookClient(t)
	alice := forge.User{ID: 42, Username: "alice"}
	sha := strings.Repeat("ab", 20)

	ev := one(t, c, gitlabtest.PushHook(testSecret, "feature/x", sha, alice))
	if ev.Type != forge.EventPush || ev.Branch != "feature/x" || ev.SHA != sha || ev.Pusher != alice || ev.Name != "Push Hook" {
		t.Errorf("push: %+v", ev)
	}
	ev = one(t, c, gitlabtest.PushHook(testSecret, "feature/x", strings.Repeat("0", 40), alice))
	if ev.Type != forge.EventIgnored {
		t.Errorf("delete: %+v", ev)
	}

	mr := forge.MR{IID: 9, SourceBranch: "fix", HeadSHA: sha, FromFork: true}
	for action, want := range map[string]forge.EventType{
		"open": forge.EventMR, "reopen": forge.EventMR, "close": forge.EventMR, "merge": forge.EventMR,
		"update": forge.EventIgnored, "approved": forge.EventIgnored,
	} {
		ev = one(t, c, gitlabtest.MRHook(testSecret, mr, action))
		if ev.Type != want {
			t.Errorf("mr %s: type %v, want %v", action, ev.Type, want)
			continue
		}
		if want == forge.EventMR && (ev.MRAction != action || ev.MR.IID != 9 || ev.MR.HeadSHA != sha || !ev.MR.FromFork || ev.MR.SourceBranch != "fix") {
			t.Errorf("mr %s: %+v", action, ev)
		}
	}
	ev = one(t, c, gitlabtest.MRHook(testSecret, mr, "merge"))
	if ev.MR.State != "merged" {
		t.Errorf("merge state %q", ev.MR.State)
	}

	ev = one(t, c, gitlabtest.NoteHook(testSecret, 9, alice, "/ansibled check"))
	if ev.Type != forge.EventComment || ev.MRIID != 9 || ev.Author != alice || ev.Body != "/ansibled check" {
		t.Errorf("note: %+v", ev)
	}

	if _, err := c.HandleWebhook(gitlabtest.PushHook("nope", "main", sha, alice)); !errors.Is(err, forge.ErrUnauthorized) {
		t.Errorf("wrong secret: %v", err)
	}
	r := gitlabtest.NoteHook(testSecret, 9, alice, "x")
	if r.Method != http.MethodPost || r.URL.Path != "/webhook/gitlab" {
		t.Errorf("helper request %s %s", r.Method, r.URL.Path)
	}
}

func TestWebhookServerHelpers(t *testing.T) {
	srv := gitlabtest.New()
	defer srv.Close()
	srv.SetProject("myorg/infra", 17, "main")
	srv.AddMR(forge.MR{IID: 4, SourceBranch: "topic", HeadSHA: "abc"})
	c := hookClient(t)
	ev := one(t, c, srv.NoteHook(testSecret, 4, forge.User{ID: 1, Username: "x"}, "hi"))
	if ev.MR.SourceBranch != "topic" || ev.MR.HeadSHA != "abc" {
		t.Errorf("server note hook: %+v", ev)
	}
}

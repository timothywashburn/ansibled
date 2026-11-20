package gitlabtest

import (
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestNewWithListener(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := NewWithListener(l)
	defer s.Close()
	if want := "http://" + l.Addr().String(); s.URL() != want {
		t.Fatalf("URL() = %q, want %q", s.URL(), want)
	}
	req, _ := http.NewRequest(http.MethodGet, s.URL()+"/api/v4/user", nil)
	req.Header.Set("PRIVATE-TOKEN", Token)
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /user: %d", resp.StatusCode)
	}
	if got := s.Requests(); len(got) != 1 || !strings.HasPrefix(got[0], "GET /api/v4/user") {
		t.Fatalf("requests %v", got)
	}
}

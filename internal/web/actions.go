package web

import (
	"crypto/hmac"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/timothywashburn/ansibled/internal/daemon"
)

// csrfToken is the form token of a session: a MAC over the session's subject
// and expiry, so it changes with every sign-in and needs no storage.
func (s *Server) csrfToken(sess *session) string {
	if sess == nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(s.mac("csrf", sess.Subject+"\x00"+strconv.FormatInt(sess.Expires, 10)))
}

// checkForm verifies that a POST came from one of our own pages: the browser
// must not label it cross-site, and it must carry the session's token.
func (s *Server) checkForm(w http.ResponseWriter, r *http.Request, sess *session) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(r.PostForm.Get("csrf"))
	if err != nil {
		return false
	}
	want, _ := base64.RawURLEncoding.DecodeString(s.csrfToken(sess))
	return hmac.Equal(got, want)
}

// action checks the request and the run id shared by the buttons.
func (s *Server) action(w http.ResponseWriter, r *http.Request, sess *session) (int64, bool) {
	if s.o.Actions == nil {
		s.message(w, r, sess, http.StatusNotFound, "Not available", "Actions are not enabled on this instance.", "/", "Overview")
		return 0, false
	}
	if !s.checkForm(w, r, sess) {
		s.log.Warn("rejected form post", "user", sess.Username, "path", r.URL.Path, "remote", r.RemoteAddr)
		s.message(w, r, sess, http.StatusForbidden, "Request rejected",
			"The form was stale or did not come from this site. Reload the page and try again.", "", "")
		return 0, false
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.message(w, r, sess, http.StatusNotFound, "Not found", "There is no such run.", "/runs", "All runs")
		return 0, false
	}
	return id, true
}

func (s *Server) actionFailed(w http.ResponseWriter, r *http.Request, sess *session, id int64, what string, err error) {
	msg := "Could not " + what + "; see the daemon log."
	status := http.StatusInternalServerError
	var ae daemon.ActionError
	if errors.As(err, &ae) {
		msg = "Could not " + what + ": " + ae.Error() + "."
		status = http.StatusConflict
	} else {
		s.log.Error(what, "run", id, "user", sess.Username, "err", err)
	}
	s.message(w, r, sess, status, "Not done", msg, fmt.Sprintf("/runs/%d", id), "Back to the run")
}

// rerun queues a run again and redirects to the new run.
func (s *Server) rerun(w http.ResponseWriter, r *http.Request, sess *session) {
	id, ok := s.action(w, r, sess)
	if !ok {
		return
	}
	nr, err := s.o.Actions.Rerun(r.Context(), id, sess.Username)
	if err != nil {
		s.actionFailed(w, r, sess, id, "re-run", err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/runs/%d#log", nr.ID), http.StatusSeeOther)
}

// cancel cancels a run and redirects back to it.
func (s *Server) cancel(w http.ResponseWriter, r *http.Request, sess *session) {
	id, ok := s.action(w, r, sess)
	if !ok {
		return
	}
	if err := s.o.Actions.Cancel(r.Context(), id, sess.Username); err != nil {
		s.actionFailed(w, r, sess, id, "cancel", err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/runs/%d", id), http.StatusSeeOther)
}

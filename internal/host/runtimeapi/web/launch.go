package web

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

const launchQueryParameter = "launch"

// LaunchCodeResult is returned to capability-token holders that open the
// browser workspace. The code is redeemed once at "/?launch=<code>".
type LaunchCodeResult struct {
	Code             string `json:"code"`
	ExpiresInSeconds int    `json:"expires_in_seconds"`
}

// IssueLaunchCode mints a one-time code that a browser exchanges for the
// HttpOnly session cookie. Unredeemed codes expire after
// Capacity.LaunchCodeTTLSeconds.
func (s *Server) IssueLaunchCode() (string, error) {
	code, err := newToken()
	if err != nil {
		return "", err
	}
	now := s.now()
	s.launchMu.Lock()
	defer s.launchMu.Unlock()
	for existing, expiresAt := range s.launchCodes {
		if !now.Before(expiresAt) {
			delete(s.launchCodes, existing)
		}
	}
	s.launchCodes[code] = now.Add(
		time.Duration(s.capacity.LaunchCodeTTLSeconds) * time.Second,
	)
	return code, nil
}

// LaunchURL returns rawURL with a fresh launch code added to its query.
func (s *Server) LaunchURL(rawURL string) (string, error) {
	code, err := s.IssueLaunchCode()
	if err != nil {
		return "", err
	}
	return WithLaunchCode(rawURL, code)
}

// WithLaunchCode appends code as the launch parameter, keeping the existing
// query order so the workspace selection stays readable in printed URLs.
func WithLaunchCode(rawURL, code string) (string, error) {
	target, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	parameter := launchQueryParameter + "=" + url.QueryEscape(code)
	if target.RawQuery == "" {
		target.RawQuery = parameter
	} else {
		target.RawQuery += "&" + parameter
	}
	return target.String(), nil
}

func (s *Server) redeemLaunchCode(code string) bool {
	if code == "" {
		return false
	}
	s.launchMu.Lock()
	expiresAt, found := s.launchCodes[code]
	delete(s.launchCodes, code)
	s.launchMu.Unlock()
	return found && s.now().Before(expiresAt)
}

// launch redeems a launch code and always redirects to "/" so the code leaves
// the address bar and history; an unknown or expired code sets no cookie.
func (s *Server) launch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	code := query.Get(launchQueryParameter)
	query.Del(launchQueryParameter)
	if s.redeemLaunchCode(code) {
		http.SetCookie(w, &http.Cookie{
			Name:     s.sessionCookieName(),
			Value:    s.session,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
		})
	}
	target := url.URL{Path: "/", RawQuery: query.Encode()}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target.String(), http.StatusSeeOther)
}

// sessionCookieName is scoped by port because browsers share cookies across
// every port of 127.0.0.1.
func (s *Server) sessionCookieName() string {
	_, port, err := net.SplitHostPort(s.expectedHost)
	if err != nil || port == "" {
		return "qcode_session"
	}
	return "qcode_session_" + port
}

func (s *Server) authorized(r *http.Request) bool {
	if _, present := r.Header["Authorization"]; present {
		return s.bearerAuthorized(r)
	}
	cookie, err := r.Cookie(s.sessionCookieName())
	return err == nil && tokenEqual(cookie.Value, s.session)
}

func (s *Server) bearerAuthorized(r *http.Request) bool {
	const prefix = "Bearer "
	value := r.Header.Get("Authorization")
	return strings.HasPrefix(value, prefix) &&
		tokenEqual(strings.TrimPrefix(value, prefix), s.token)
}

// authLaunchCode serves owners that reach the Web host out of process (a
// repeated qcode invocation or the desktop shell). Browser sessions cannot
// mint codes: only the owner-lease capability token can.
func (s *Server) authLaunchCode(r *http.Request, _ Dependencies) (any, error) {
	if !s.bearerAuthorized(r) {
		return nil, protocol.NewProblem(
			protocol.CodeUnavailable,
			"launch codes require the owner capability token",
			false,
			nil,
		)
	}
	if err := s.decodeRequest(r, &struct{}{}); err != nil {
		return nil, err
	}
	code, err := s.IssueLaunchCode()
	if err != nil {
		return nil, err
	}
	return LaunchCodeResult{
		Code:             code,
		ExpiresInSeconds: s.capacity.LaunchCodeTTLSeconds,
	}, nil
}

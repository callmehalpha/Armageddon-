package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/callmehalpha/Armageddon-/internal/identity"
	"github.com/callmehalpha/Armageddon-/internal/ids"
	"github.com/callmehalpha/Armageddon-/internal/store"
)

const (
	sessionCookie   = "arm_session"
	sessionIdle     = 12 * time.Hour
	sessionAbsolute = 7 * 24 * time.Hour
	reauthWindow    = 10 * time.Minute
	deviceTokenTTL  = 15 * time.Minute
	pairingTTL      = 10 * time.Minute
)

type ctxKey int

const (
	ctxUser ctxKey = iota
	ctxSession
	ctxDevice
)

func userOf(r *http.Request) *store.User {
	u, _ := r.Context().Value(ctxUser).(*store.User)
	return u
}

func sessionOf(r *http.Request) *store.Session {
	x, _ := r.Context().Value(ctxSession).(*store.Session)
	return x
}

func deviceOf(r *http.Request) *store.Device {
	d, _ := r.Context().Value(ctxDevice).(*store.Device)
	return d
}

func writeJSON(rw http.ResponseWriter, code int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(code)
	json.NewEncoder(rw).Encode(v)
}

func writeErr(rw http.ResponseWriter, code int, msg string) {
	writeJSON(rw, code, map[string]string{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// ---- browser sessions ----

func (s *Server) startSession(rw http.ResponseWriter, r *http.Request, u *store.User) (*store.Session, error) {
	tok := ids.Secret(32)
	now := store.Now()
	x := &store.Session{ID: ids.New(), UserID: u.ID, TokenHash: ids.Hash(tok), CSRF: ids.Secret(24),
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now + sessionAbsolute.Milliseconds(), ReauthAt: now}
	if err := s.store.CreateSession(x); err != nil {
		return nil, err
	}
	http.SetCookie(rw, &http.Cookie{Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true,
		Secure: r.TLS != nil || strings.HasPrefix(s.cfg.PublicURL, "https://"), SameSite: http.SameSiteLaxMode,
		Expires: time.UnixMilli(x.ExpiresAt)})
	return x, nil
}

// withSession attaches the logged-in user, if any. Mutating requests must
// carry the session's CSRF token in X-CSRF-Token.
func (s *Server) withSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err == nil {
			x, err := s.store.SessionByTokenHash(ids.Hash(c.Value))
			now := store.Now()
			if err == nil && now < x.ExpiresAt && now-x.LastSeenAt < sessionIdle.Milliseconds() {
				if u, err := s.store.UserByID(x.UserID); err == nil && !u.Disabled {
					if r.Method != http.MethodGet && r.Method != http.MethodHead {
						// code-server and proxied apps cannot send our CSRF
						// header; their requests must come from this origin
						// instead.
						if isIDEPath(r.URL.Path) || isPortPath(r.URL.Path) {
							if !sameOrigin(r) {
								writeErr(rw, http.StatusForbidden, "cross-origin request refused")
								return
							}
						} else if r.Header.Get("X-CSRF-Token") != x.CSRF {
							writeErr(rw, http.StatusForbidden, "missing or invalid CSRF token")
							return
						}
					}
					if now-x.LastSeenAt > time.Minute.Milliseconds() {
						s.store.TouchSession(x.ID, now)
					}
					ctx := context.WithValue(r.Context(), ctxUser, u)
					ctx = context.WithValue(ctx, ctxSession, x)
					r = r.WithContext(ctx)
				}
			}
		}
		next.ServeHTTP(rw, r)
	})
}

// requireUser accepts a browser session or a device token (acting as the
// device's user).
func (s *Server) requireUser(h http.HandlerFunc) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		if userOf(r) == nil {
			if d := s.deviceFromRequest(r); d != nil {
				u, err := s.store.UserByID(d.UserID)
				if err == nil && !u.Disabled {
					ctx := context.WithValue(r.Context(), ctxUser, u)
					r = r.WithContext(context.WithValue(ctx, ctxDevice, d))
				}
			}
		}
		if userOf(r) == nil {
			writeErr(rw, http.StatusUnauthorized, "login required")
			return
		}
		h(rw, r)
	}
}

func (s *Server) requireAdmin(h http.HandlerFunc) http.HandlerFunc {
	return s.requireUser(func(rw http.ResponseWriter, r *http.Request) {
		if userOf(r).Role != "admin" {
			writeErr(rw, http.StatusForbidden, "admin only")
			return
		}
		h(rw, r)
	})
}

var usernameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,31}$`)

func (s *Server) handleSetupStatus(rw http.ResponseWriter, r *http.Request) {
	n, _ := s.store.CountUsers()
	writeJSON(rw, 200, map[string]bool{"needs_setup": n == 0})
}

// handleSetup consumes the one-time setup token and creates the first admin.
func (s *Server) handleSetup(rw http.ResponseWriter, r *http.Request) {
	s.createUserWithToken(rw, r, "setup")
}

func (s *Server) handleAcceptInvite(rw http.ResponseWriter, r *http.Request) {
	s.createUserWithToken(rw, r, "invite")
}

func (s *Server) createUserWithToken(rw http.ResponseWriter, r *http.Request, kind string) {
	var req struct{ Token, Username, Password string }
	if err := readJSON(r, &req); err != nil {
		writeErr(rw, 400, "bad request")
		return
	}
	req.Username = strings.ToLower(strings.TrimSpace(req.Username))
	if !usernameRe.MatchString(req.Username) {
		writeErr(rw, 400, "username: 2-32 characters, lowercase letters, digits, - or _, starting with a letter")
		return
	}
	hash, err := identity.HashPassword(req.Password)
	if err != nil {
		writeErr(rw, 400, err.Error())
		return
	}
	u := &store.User{ID: ids.New(), Username: req.Username, PasswordHash: hash, CreatedAt: store.Now()}
	err = s.store.Tx(r.Context(), func(tx *sql.Tx) error {
		role, err := s.store.ConsumeOneTimeToken(tx, ids.Hash(req.Token), kind, store.Now())
		if err != nil {
			return err
		}
		u.Role = role
		return s.store.CreateUserTx(tx, u)
	})
	if errors.Is(err, store.ErrNotFound) {
		writeErr(rw, 403, "this link is invalid, expired or already used")
		return
	}
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeErr(rw, 409, "username already taken")
			return
		}
		writeErr(rw, 500, err.Error())
		return
	}
	s.event("", "user", u.ID, "user.created", map[string]string{"username": u.Username, "via": kind})
	x, err := s.startSession(rw, r, u)
	if err != nil {
		writeErr(rw, 500, err.Error())
		return
	}
	writeJSON(rw, 200, map[string]any{"user": publicUser(u), "csrf": x.CSRF})
}

func publicUser(u *store.User) map[string]string {
	return map[string]string{"id": u.ID, "username": u.Username, "role": u.Role}
}

func (s *Server) handleLogin(rw http.ResponseWriter, r *http.Request) {
	var req struct{ Username, Password string }
	if err := readJSON(r, &req); err != nil {
		writeErr(rw, 400, "bad request")
		return
	}
	u, err := s.store.UserByName(strings.ToLower(strings.TrimSpace(req.Username)))
	if err != nil || u.Disabled || !identity.VerifyPassword(u.PasswordHash, req.Password) {
		time.Sleep(300 * time.Millisecond) // blunt online guessing
		writeErr(rw, 401, "wrong username or password")
		return
	}
	x, err := s.startSession(rw, r, u)
	if err != nil {
		writeErr(rw, 500, err.Error())
		return
	}
	writeJSON(rw, 200, map[string]any{"user": publicUser(u), "csrf": x.CSRF})
}

func (s *Server) handleLogout(rw http.ResponseWriter, r *http.Request) {
	if x := sessionOf(r); x != nil {
		s.store.DeleteSession(x.ID)
	}
	http.SetCookie(rw, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(rw, 200, map[string]bool{"ok": true})
}

func (s *Server) handleMe(rw http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	if u == nil {
		// 200 with no user: "am I logged in?" is not an error for the UI.
		writeJSON(rw, 200, map[string]any{"user": nil})
		return
	}
	out := map[string]any{"user": publicUser(u)}
	if x := sessionOf(r); x != nil {
		out["csrf"] = x.CSRF
	}
	writeJSON(rw, 200, out)
}

func (s *Server) handleCreateInvite(rw http.ResponseWriter, r *http.Request) {
	tok := ids.Secret(32)
	if err := s.store.CreateOneTimeToken(ids.Hash(tok), "invite", "user", userOf(r).ID, store.Now()+(72*time.Hour).Milliseconds()); err != nil {
		writeErr(rw, 500, err.Error())
		return
	}
	writeJSON(rw, 200, map[string]string{"url": s.cfg.PublicURL + "/invite?token=" + tok, "expires_in": "72h"})
}

func (s *Server) handleUsers(rw http.ResponseWriter, r *http.Request) {
	us, err := s.store.ListUsers()
	if err != nil {
		writeErr(rw, 500, err.Error())
		return
	}
	var out []map[string]string
	for _, u := range us {
		out = append(out, publicUser(u))
	}
	writeJSON(rw, 200, out)
}

// ---- devices: pairing (device-code flow, §7.3) and tokens ----

func (s *Server) handlePairStart(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		PublicKey string `json:"public_key"`
		Name      string `json:"name"`
		Platform  string `json:"platform"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(rw, 400, "bad request")
		return
	}
	if _, err := identity.ParsePublicKey(req.PublicKey); err != nil {
		writeErr(rw, 400, err.Error())
		return
	}
	if req.Name == "" || len(req.Name) > 64 {
		writeErr(rw, 400, "device name required (max 64 characters)")
		return
	}
	code, secret := ids.UserCode(), ids.Secret(32)
	p := &store.Pairing{ID: ids.New(), UserCodeHash: ids.Hash(code), PollSecretHash: ids.Hash(secret), PublicKey: req.PublicKey,
		Name: req.Name, Platform: req.Platform, ExpiresAt: store.Now() + pairingTTL.Milliseconds()}
	if err := s.store.CreatePairing(p); err != nil {
		writeErr(rw, 500, err.Error())
		return
	}
	writeJSON(rw, 200, map[string]any{"pairing_id": p.ID, "poll_secret": secret, "user_code": code,
		"verification_url": s.cfg.PublicURL + "/pair?code=" + code, "expires_in": int(pairingTTL.Seconds())})
}

func (s *Server) handlePairPoll(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		PairingID  string `json:"pairing_id"`
		PollSecret string `json:"poll_secret"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(rw, 400, "bad request")
		return
	}
	p, err := s.store.PairingByID(req.PairingID)
	if err != nil || p.PollSecretHash != ids.Hash(req.PollSecret) {
		writeErr(rw, 404, "unknown pairing")
		return
	}
	switch {
	case p.DeviceID != "":
		writeJSON(rw, 200, map[string]string{"status": "approved", "device_id": p.DeviceID})
	case store.Now() > p.ExpiresAt:
		writeJSON(rw, 200, map[string]string{"status": "expired"})
	default:
		writeJSON(rw, 200, map[string]string{"status": "pending"})
	}
}

// handlePairLookup shows the approval page what it is approving.
func (s *Server) handlePairLookup(rw http.ResponseWriter, r *http.Request) {
	p, err := s.store.PairingByCode(ids.Hash(strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("code")))))
	if err != nil || store.Now() > p.ExpiresAt || p.DeviceID != "" {
		writeErr(rw, 404, "this pairing code is invalid, expired or already used")
		return
	}
	writeJSON(rw, 200, map[string]string{"name": p.Name, "platform": p.Platform, "fingerprint": identity.Fingerprint(p.PublicKey)})
}

func (s *Server) handlePairApprove(rw http.ResponseWriter, r *http.Request) {
	if sessionOf(r) == nil {
		writeErr(rw, 403, "approve pairings from a browser session")
		return
	}
	var req struct{ Code string }
	if err := readJSON(r, &req); err != nil {
		writeErr(rw, 400, "bad request")
		return
	}
	p, err := s.store.PairingByCode(ids.Hash(strings.ToUpper(strings.TrimSpace(req.Code))))
	if err != nil {
		writeErr(rw, 404, "unknown pairing code")
		return
	}
	devID := ids.New()
	if err := s.store.ApprovePairing(p, userOf(r).ID, devID, store.Now()); err != nil {
		writeErr(rw, 409, err.Error())
		return
	}
	s.event("", "user", userOf(r).ID, "device.paired", map[string]string{"device_id": devID, "name": p.Name})
	writeJSON(rw, 200, map[string]string{"device_id": devID, "name": p.Name})
}

func (s *Server) handleChallenge(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceID string `json:"device_id"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(rw, 400, "bad request")
		return
	}
	d, err := s.store.DeviceByID(req.DeviceID)
	if err != nil || d.Revoked {
		writeErr(rw, 401, "unknown or revoked device")
		return
	}
	nonce := ids.Secret(24)
	if err := s.store.CreateNonce(nonce, d.ID, store.Now()+time.Minute.Milliseconds()); err != nil {
		writeErr(rw, 500, err.Error())
		return
	}
	writeJSON(rw, 200, map[string]string{"nonce": nonce})
}

func (s *Server) handleToken(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceID  string `json:"device_id"`
		Nonce     string `json:"nonce"`
		Signature string `json:"signature"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(rw, 400, "bad request")
		return
	}
	d, err := s.store.DeviceByID(req.DeviceID)
	if err != nil || d.Revoked {
		writeErr(rw, 401, "unknown or revoked device")
		return
	}
	pub, err := identity.ParsePublicKey(d.PublicKey)
	if err != nil || s.store.ConsumeNonce(req.Nonce, d.ID, store.Now()) != nil || !identity.VerifyChallenge(pub, d.ID, req.Nonce, req.Signature) {
		writeErr(rw, 401, "challenge failed")
		return
	}
	tok := ids.Secret(32)
	if err := s.store.CreateDeviceToken(ids.Hash(tok), d.ID, store.Now()+deviceTokenTTL.Milliseconds()); err != nil {
		writeErr(rw, 500, err.Error())
		return
	}
	s.store.TouchDevice(d.ID, store.Now())
	writeJSON(rw, 200, map[string]any{"access_token": tok, "expires_in": int(deviceTokenTTL.Seconds())})
}

// deviceFromRequest resolves a device access token from a Bearer header or
// HTTP Basic auth (password = token; what git's credential helper sends).
func (s *Server) deviceFromRequest(r *http.Request) *store.Device {
	var tok string
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		tok = strings.TrimPrefix(h, "Bearer ")
	} else if _, pw, ok := r.BasicAuth(); ok {
		tok = pw
	}
	if tok == "" {
		return nil
	}
	d, err := s.store.DeviceForToken(ids.Hash(tok), store.Now())
	if err != nil {
		return nil
	}
	return d
}

func (s *Server) handleDevices(rw http.ResponseWriter, r *http.Request) {
	ds, err := s.store.DevicesOfUser(userOf(r).ID)
	if err != nil {
		writeErr(rw, 500, err.Error())
		return
	}
	out := []map[string]any{}
	for _, d := range ds {
		out = append(out, map[string]any{"id": d.ID, "name": d.Name, "platform": d.Platform, "revoked": d.Revoked,
			"last_seen_at": d.LastSeenAt, "fingerprint": identity.Fingerprint(d.PublicKey)})
	}
	writeJSON(rw, 200, out)
}

func (s *Server) handleRevokeDevice(rw http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.RevokeDevice(id, userOf(r).ID, store.Now()); err != nil {
		writeErr(rw, 404, "no such device")
		return
	}
	s.sshd.closeDevice(id, "this device was revoked")
	s.event("", "user", userOf(r).ID, "device.revoked", map[string]string{"device_id": id})
	// A revoked device that holds a lease loses it at once (§3.2, F12).
	s.revokeLeases(id, userOf(r).ID)
	writeJSON(rw, 200, map[string]bool{"ok": true})
}

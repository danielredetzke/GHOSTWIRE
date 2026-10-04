package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Users of the web interface. Every user is an admin; the only rules are
// that nobody deletes themselves (so one user always remains) and that
// everyone changes their own password with the current one.

type userView struct {
	ID                 string         `json:"id"`
	Username           string         `json:"username"`
	Note               string         `json:"note"`
	MustChangePassword bool           `json:"mustChangePassword"`
	Created            time.Time      `json:"created"`
	LastLogin          *tokenUse      `json:"lastLogin"` // since the service started
	Tokens             int            `json:"tokens"`
	You                bool           `json:"you"`
	MFA                map[string]any `json:"mfa"` // {"totp": bool, "passkeys": n}
}

func (a *App) userView(c *Config, u *User, me string) userView {
	n := 0
	for _, t := range c.APITokens {
		if t.UserID == u.ID {
			n++
		}
	}
	return userView{u.ID, u.Username, u.Note, u.MustChangePassword, u.Created, a.auth.LastLogin(u.ID), n, u.ID == me, mfaSummary(u)}
}

// username names a user for lists, or "" if the ID is unknown.
func (a *App) username(c *Config, id string) string {
	if _, u := c.userByID(id); u != nil {
		return u.Username
	}
	return ""
}

func (a *App) listUsers(w http.ResponseWriter, r *http.Request) {
	cfg := a.store.Get()
	out := make([]userView, 0, len(cfg.Users))
	for i := range cfg.Users {
		out = append(out, a.userView(cfg, &cfg.Users[i], who(r).UserID))
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

// newPasswordHash checks and hashes a password an admin chose for someone.
func newPasswordHash(pw string) (string, error) {
	if err := validatePassword(pw); err != nil {
		return "", err
	}
	return hashPassword(pw)
}

func (a *App) createUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username           string `json:"username"`
		Note               string `json:"note"`
		Password           string `json:"password"`
		MustChangePassword *bool  `json:"mustChangePassword"` // default true
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	hash, err := newPasswordHash(in.Password)
	if err != nil {
		writeErr(w, err)
		return
	}
	u := User{
		ID: newID(), Username: strings.TrimSpace(in.Username), Note: strings.TrimSpace(in.Note),
		PasswordHash: hash, MustChangePassword: in.MustChangePassword == nil || *in.MustChangePassword,
		Created: time.Now().UTC(),
	}
	if err := a.store.Update(func(c *Config) error {
		if c.userByName(u.Username) != nil {
			return badRequest("username %q is taken", u.Username)
		}
		c.Users = append(c.Users, u)
		return nil
	}); err != nil {
		writeErr(w, err)
		return
	}
	a.audit(r, "user created", "user", u.Username, "mustChangePassword", u.MustChangePassword)
	cfg := a.store.Get()
	_, saved := cfg.userByID(u.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"user": a.userView(cfg, saved, who(r).UserID)})
}

// patchUser renames a user, changes the note, or sets or clears the
// "must change password" flag.
func (a *App) patchUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m, err := decodeFields(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var name string
	var changed []string
	err = a.store.Update(func(c *Config) error {
		_, u := c.userByID(id)
		if u == nil {
			return badRequest("no such user")
		}
		for _, f := range []struct {
			key string
			dst any
		}{
			{"username", &u.Username}, {"note", &u.Note}, {"mustChangePassword", &u.MustChangePassword},
		} {
			if raw, ok := m[f.key]; ok {
				if err := json.Unmarshal(raw, f.dst); err != nil {
					return badRequest("%s: %v", f.key, err)
				}
				changed = append(changed, f.key)
			}
		}
		u.Username = strings.TrimSpace(u.Username)
		u.Note = strings.TrimSpace(u.Note)
		if other := c.userByName(u.Username); other != nil && other.ID != u.ID {
			return badRequest("username %q is taken", u.Username)
		}
		name = u.Username
		return nil
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	a.audit(r, "user updated", "user", name, "fields", changed)
	cfg := a.store.Get()
	_, u := cfg.userByID(id)
	writeJSON(w, http.StatusOK, map[string]any{"user": a.userView(cfg, u, who(r).UserID)})
}

// resetPassword sets a password for another user. Their sessions end,
// because sessions are tied to the password they started with.
func (a *App) resetPassword(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == who(r).UserID {
		writeErr(w, badRequest("change your own password under My account"))
		return
	}
	var in struct {
		Password           string `json:"password"`
		MustChangePassword *bool  `json:"mustChangePassword"` // default true
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	hash, err := newPasswordHash(in.Password)
	if err != nil {
		writeErr(w, err)
		return
	}
	must := in.MustChangePassword == nil || *in.MustChangePassword
	var name string
	if err := a.store.Update(func(c *Config) error {
		_, u := c.userByID(id)
		if u == nil {
			return badRequest("no such user")
		}
		u.PasswordHash, u.MustChangePassword = hash, must
		name = u.Username
		return nil
	}); err != nil {
		writeErr(w, err)
		return
	}
	a.audit(r, "user password reset", "user", name, "mustChangePassword", must)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// deleteUser removes a user and their API tokens.
func (a *App) deleteUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == who(r).UserID {
		writeErr(w, badRequest("you cannot delete yourself"))
		return
	}
	var name string
	var tokens int
	if err := a.store.Update(func(c *Config) error {
		i, u := c.userByID(id)
		if u == nil {
			return badRequest("no such user")
		}
		name = u.Username
		n := len(c.APITokens)
		c.APITokens = slices.DeleteFunc(c.APITokens, func(t APIToken) bool { return t.UserID == id })
		tokens = n - len(c.APITokens)
		c.Users = slices.Delete(c.Users, i, i+1)
		return nil
	}); err != nil {
		writeErr(w, err)
		return
	}
	a.audit(r, "user deleted", "user", name, "tokensRevoked", tokens)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

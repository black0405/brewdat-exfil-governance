package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/mail"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	maxFiles   = 10
	sessionTTL = 12 * time.Hour
)

var allowedExt = map[string]bool{
	".pdf": true, ".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".txt": true, ".csv": true,
	".log": true, ".json": true, ".xml": true, ".docx": true, ".xlsx": true, ".pptx": true,
	".msg": true, ".eml": true, ".zip": true,
}

var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("brewdat timing guard"), 12)

func validEmail(e string) bool {
	addr, err := mail.ParseAddress(e)
	return err == nil && addr.Address == e && len(e) <= 254
}

// flashErr and flashSticky mark flash messages that render as banners instead of toasts.
func flashErr(msg string) string    { return "!" + msg }
func flashSticky(msg string) string { return "*" + msg }

func (a *App) canAccess(u *User, controlID int64) bool {
	if u.IsAdmin() {
		var ok int
		a.db.QueryRow(`SELECT 1 FROM controls WHERE id = ?`, controlID).Scan(&ok)
		return ok == 1
	}
	var ok int
	a.db.QueryRow(`SELECT 1 FROM control_owners WHERE control_id = ? AND user_id = ?`, controlID, u.ID).Scan(&ok)
	return ok == 1
}

// ---------- sign in ----------

func (a *App) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := a.sessionUser(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.page(w, r, "login", View{Title: "Sign in"})
}

func (a *App) loginSubmit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	r.ParseForm()
	ip, now := a.clientIP(r), a.now()
	email := strings.TrimSpace(r.PostForm.Get("email"))
	v := View{Title: "Sign in", D: map[string]any{"Email": email}}
	if a.lim.blocked(ip, now) {
		v.Err = "Too many attempts from this address. Wait 15 minutes and try again."
		a.render(w, r, http.StatusTooManyRequests, "login", v)
		return
	}
	var id int64
	var hash string
	err := a.db.QueryRow(`SELECT id, password_hash FROM users WHERE email = ? AND active = 1`, email).Scan(&id, &hash)
	if err != nil {
		bcrypt.CompareHashAndPassword(dummyHash, []byte(r.PostForm.Get("password")))
	} else {
		err = bcrypt.CompareHashAndPassword([]byte(hash), []byte(r.PostForm.Get("password")))
	}
	if err != nil {
		a.lim.fail(ip, now)
		v.Err = "Email or password is incorrect."
		a.render(w, r, http.StatusUnauthorized, "login", v)
		return
	}
	a.lim.reset(ip)
	tok := token(32)
	if _, err := a.db.Exec(`INSERT INTO sessions (token_hash, user_id, csrf, expires_at) VALUES (?, ?, ?, ?)`,
		hashToken(tok), id, token(16), now.Add(sessionTTL).Unix()); err != nil {
		a.fail(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "session", Value: tok, Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, Secure: a.cfg.Secure, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("session"); err == nil {
		a.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, hashToken(c.Value))
	}
	a.clearCookie(w, "session")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---------- account ----------

func (a *App) accountPage(w http.ResponseWriter, r *http.Request) {
	a.page(w, r, "account", View{Title: "Account", Nav: "account"})
}

func (a *App) changePassword(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	cur, next := r.PostForm.Get("current"), r.PostForm.Get("new")
	fail := func(msg string) { a.redirect(w, r, "/account", flashErr(msg)) }
	var hash string
	a.db.QueryRow(`SELECT password_hash FROM users WHERE id = ?`, u.ID).Scan(&hash)
	switch {
	case bcrypt.CompareHashAndPassword([]byte(hash), []byte(cur)) != nil:
		fail("Your current password is not correct.")
		return
	case len(next) < 10 || len(next) > 72:
		fail("Choose a new password between 10 and 72 characters.")
		return
	case next != r.PostForm.Get("confirm"):
		fail("The new passwords do not match.")
		return
	case next == cur:
		fail("Choose a password you have not used here before.")
		return
	}
	nh, err := bcrypt.GenerateFromPassword([]byte(next), 12)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	c, _ := r.Cookie("session")
	a.db.Exec(`UPDATE users SET password_hash = ?, must_change = 0 WHERE id = ?`, string(nh), u.ID)
	a.db.Exec(`DELETE FROM sessions WHERE user_id = ? AND token_hash <> ?`, u.ID, hashToken(c.Value))
	a.redirect(w, r, "/", "Password changed.")
}

// ---------- dashboard ----------

type ControlRow struct {
	ID                  int64
	Code, Title, Owners string
	Count               int
	Last                int64
	Status, Class       string
}

func status(count, period, current int) (string, string) {
	switch {
	case count > 0:
		return "Submitted", "ok"
	case period == current:
		return "Due", "due"
	}
	return "Missing", "late"
}

func (a *App) dashboard(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	now := a.now()
	cur := periodOf(now)
	periods := recentPeriods(now, 12)
	p := cur
	if q, err := strconv.Atoi(r.URL.Query().Get("period")); err == nil {
		for _, x := range periods {
			if x == q {
				p = q
			}
		}
	}
	isAdmin := 0
	if u.IsAdmin() {
		isAdmin = 1
	}
	rows, err := a.db.Query(`SELECT c.id, c.code, c.title,
		(SELECT COUNT(*) FROM submissions s WHERE s.control_id = c.id AND s.period = ?1),
		COALESCE((SELECT MAX(s.created_at) FROM submissions s WHERE s.control_id = c.id AND s.period = ?1), 0),
		COALESCE((SELECT group_concat(u.name, ', ') FROM control_owners o JOIN users u ON u.id = o.user_id WHERE o.control_id = c.id), '')
		FROM controls c
		WHERE ?2 = 1 OR c.id IN (SELECT control_id FROM control_owners WHERE user_id = ?3)
		ORDER BY c.code`, p, isAdmin, u.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	defer rows.Close()
	var list []ControlRow
	done := 0
	for rows.Next() {
		var c ControlRow
		if err := rows.Scan(&c.ID, &c.Code, &c.Title, &c.Count, &c.Last, &c.Owners); err != nil {
			a.fail(w, r, err)
			return
		}
		c.Status, c.Class = status(c.Count, p, cur)
		if c.Count > 0 {
			done++
		}
		list = append(list, c)
	}
	a.page(w, r, "dashboard", View{Title: "Overview", Nav: "home", D: map[string]any{
		"Rows": list, "Done": done, "Total": len(list), "Open": len(list) - done,
		"Period": p, "Current": cur, "Periods": periods, "Today": now.Format("Monday 2 January"),
	}})
}

// ---------- control detail and evidence ----------

type Submission struct {
	ID        int64
	Period    int
	Note, By  string
	CreatedAt int64
	Files     []FileRow
}

type FileRow struct {
	ID   int64
	Name string
	Size int64
}

func (a *App) controlPage(w http.ResponseWriter, r *http.Request) {
	u, id := currentUser(r), pathID(r)
	if !a.canAccess(u, id) {
		a.notFound(w, r)
		return
	}
	var code, title, desc string
	if err := a.db.QueryRow(`SELECT code, title, description FROM controls WHERE id = ?`, id).Scan(&code, &title, &desc); err != nil {
		a.notFound(w, r)
		return
	}
	var owners []string
	orows, err := a.db.Query(`SELECT u.name FROM control_owners o JOIN users u ON u.id = o.user_id WHERE o.control_id = ? ORDER BY u.name`, id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	for orows.Next() {
		var n string
		orows.Scan(&n)
		owners = append(owners, n)
	}
	orows.Close()

	rows, err := a.db.Query(`SELECT s.id, s.period, s.note, s.created_at, u.name, COALESCE(f.id, 0), COALESCE(f.orig_name, ''), COALESCE(f.size, 0)
		FROM submissions s JOIN users u ON u.id = s.user_id LEFT JOIN files f ON f.submission_id = s.id
		WHERE s.control_id = ? ORDER BY s.created_at DESC, s.id DESC, f.id`, id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	defer rows.Close()
	var subs []*Submission
	submitted := map[int]bool{}
	for rows.Next() {
		var s Submission
		var f FileRow
		if err := rows.Scan(&s.ID, &s.Period, &s.Note, &s.CreatedAt, &s.By, &f.ID, &f.Name, &f.Size); err != nil {
			a.fail(w, r, err)
			return
		}
		if len(subs) == 0 || subs[len(subs)-1].ID != s.ID {
			subs = append(subs, &s)
		}
		if f.ID != 0 {
			last := subs[len(subs)-1]
			last.Files = append(last.Files, f)
		}
		submitted[s.Period] = true
	}
	now := a.now()
	cur := periodOf(now)
	n := 0
	if submitted[cur] {
		n = 1
	}
	st, cls := status(n, cur, cur)
	a.page(w, r, "control", View{Title: code, Nav: "home", D: map[string]any{
		"ID": id, "Code": code, "ControlTitle": title, "Description": desc, "Owners": strings.Join(owners, ", "),
		"Subs": subs, "Periods": recentPeriods(now, 12), "Current": cur, "Status": st, "Class": cls,
		"MaxMB": a.cfg.MaxUploadMB, "MaxFiles": maxFiles,
	}})
}

func (a *App) uploadEvidence(w http.ResponseWriter, r *http.Request) {
	u, id := currentUser(r), pathID(r)
	if !a.canAccess(u, id) {
		a.notFound(w, r)
		return
	}
	back := fmt.Sprintf("/controls/%d", id)
	fail := func(msg string) { a.redirect(w, r, back, flashErr(msg)) }

	period, _ := strconv.Atoi(r.PostForm.Get("period"))
	valid := false
	for _, p := range recentPeriods(a.now(), 12) {
		valid = valid || p == period
	}
	if !valid {
		fail("Choose a period from the list.")
		return
	}
	note := strings.TrimSpace(r.PostForm.Get("note"))
	if len(note) > 4000 {
		fail("Keep the note under 4000 characters.")
		return
	}
	if r.MultipartForm == nil || len(r.MultipartForm.File["files"]) == 0 {
		fail("Choose at least one file to upload.")
		return
	}
	headers := r.MultipartForm.File["files"]
	if len(headers) > maxFiles {
		fail(fmt.Sprintf("Upload at most %d files at a time.", maxFiles))
		return
	}
	for _, h := range headers {
		if !allowedExt[strings.ToLower(filepath.Ext(h.Filename))] {
			fail(fmt.Sprintf("%s is not an accepted file type. Use PDF, images, Office documents, text, email or zip files.", filepath.Base(h.Filename)))
			return
		}
		if h.Size > a.cfg.MaxUploadMB<<20 {
			fail(fmt.Sprintf("%s is larger than %d MB.", filepath.Base(h.Filename), a.cfg.MaxUploadMB))
			return
		}
	}

	type saved struct {
		name, stored, sum string
		size              int64
	}
	var done []saved
	cleanup := func() {
		for _, s := range done {
			os.Remove(filepath.Join(a.cfg.DataDir, "files", s.stored))
		}
	}
	for _, h := range headers {
		s, err := a.storeFile(h.Open)
		if err != nil {
			cleanup()
			a.fail(w, r, err)
			return
		}
		done = append(done, saved{name: filepath.Base(h.Filename), stored: s[0], sum: s[1], size: h.Size})
	}

	err := func() error {
		tx, err := a.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		res, err := tx.Exec(`INSERT INTO submissions (control_id, period, user_id, note, created_at) VALUES (?, ?, ?, ?, ?)`,
			id, period, u.ID, note, a.now().Unix())
		if err != nil {
			return err
		}
		sid, _ := res.LastInsertId()
		for _, s := range done {
			if _, err := tx.Exec(`INSERT INTO files (submission_id, orig_name, stored_name, size, sha256) VALUES (?, ?, ?, ?, ?)`,
				sid, s.name, s.stored, s.size, s.sum); err != nil {
				return err
			}
		}
		return tx.Commit()
	}()
	if err != nil {
		cleanup()
		a.fail(w, r, err)
		return
	}
	a.redirect(w, r, back, fmt.Sprintf("Evidence uploaded for %s.", periodLabel(period)))
}

// storeFile copies an upload to a random name under files/, returning the stored name and its sha256.
func (a *App) storeFile(open func() (multipart.File, error)) ([2]string, error) {
	src, err := open()
	if err != nil {
		return [2]string{}, err
	}
	defer src.Close()
	name := token(16)
	dir := filepath.Join(a.cfg.DataDir, "files")
	tmp := filepath.Join(dir, name+".part")
	dst, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return [2]string{}, err
	}
	sum := sha256.New()
	_, err = io.Copy(io.MultiWriter(dst, sum), src)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(dir, name))
	}
	if err != nil {
		os.Remove(tmp)
		return [2]string{}, err
	}
	return [2]string{name, hex.EncodeToString(sum.Sum(nil))}, nil
}

func (a *App) downloadFile(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var name, stored string
	var controlID int64
	err := a.db.QueryRow(`SELECT f.orig_name, f.stored_name, s.control_id FROM files f JOIN submissions s ON s.id = f.submission_id WHERE f.id = ?`,
		pathID(r)).Scan(&name, &stored, &controlID)
	if err != nil || !a.canAccess(u, controlID) {
		a.notFound(w, r)
		return
	}
	if _, err := hex.DecodeString(stored); err != nil || len(stored) != 32 {
		a.notFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(a.cfg.DataDir, "files", stored))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	defer f.Close()
	info, _ := f.Stat()
	h := w.Header()
	h.Set("Cache-Control", "private, no-store")
	ctype := previewTypes[strings.ToLower(filepath.Ext(name))]
	if r.URL.Query().Get("view") == "1" && ctype != "" {
		// Inline preview, only for types browsers render without running content.
		// Text formats are always served as plain text; everything but PDF is also sandboxed.
		h.Set("Content-Type", ctype)
		h.Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": name}))
		h.Set("X-Frame-Options", "SAMEORIGIN")
		if ctype == "application/pdf" {
			h.Set("Content-Security-Policy", "default-src 'none'; object-src 'self'; frame-ancestors 'self'")
		} else {
			h.Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self'; style-src 'unsafe-inline'; frame-ancestors 'self'")
		}
	} else {
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	}
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// previewTypes lists the uploads that can be shown in the browser instead of downloaded.
var previewTypes = map[string]string{
	".pdf": "application/pdf", ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
	".txt": "text/plain; charset=utf-8", ".log": "text/plain; charset=utf-8", ".csv": "text/plain; charset=utf-8",
	".json": "text/plain; charset=utf-8", ".xml": "text/plain; charset=utf-8",
}

// Preview tells the history list how to show a file: "pdf", "image", "text" or "" for download only.
func (f FileRow) Preview() string {
	switch t := previewTypes[strings.ToLower(filepath.Ext(f.Name))]; {
	case t == "application/pdf":
		return "pdf"
	case strings.HasPrefix(t, "image/"):
		return "image"
	case t != "":
		return "text"
	}
	return ""
}

// ---------- admin: controls ----------

type UserOption struct {
	ID          int64
	Name, Email string
	Checked     bool
}

func (a *App) activeUsers(controlID int64) ([]UserOption, error) {
	rows, err := a.db.Query(`SELECT u.id, u.name, u.email,
		EXISTS (SELECT 1 FROM control_owners o WHERE o.control_id = ? AND o.user_id = u.id)
		FROM users u WHERE u.active = 1 ORDER BY u.name`, controlID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserOption
	for rows.Next() {
		var o UserOption
		if err := rows.Scan(&o.ID, &o.Name, &o.Email, &o.Checked); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (a *App) adminControls(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT c.id, c.code, c.title,
		(SELECT COUNT(*) FROM submissions s WHERE s.control_id = c.id),
		COALESCE((SELECT group_concat(u.name, ', ') FROM control_owners o JOIN users u ON u.id = o.user_id WHERE o.control_id = c.id), '')
		FROM controls c ORDER BY c.code`)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var list []ControlRow
	for rows.Next() {
		var c ControlRow
		rows.Scan(&c.ID, &c.Code, &c.Title, &c.Count, &c.Owners)
		list = append(list, c)
	}
	rows.Close()
	users, err := a.activeUsers(0)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.page(w, r, "admin_controls", View{Title: "Controls", Nav: "controls", D: map[string]any{
		"Rows": list, "Users": users, "ID": int64(0), "Code": "", "ControlTitle": "", "Description": "",
	}})
}

func (a *App) editControl(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	var code, title, desc string
	var subs int
	err := a.db.QueryRow(`SELECT code, title, description, (SELECT COUNT(*) FROM submissions WHERE control_id = c.id) FROM controls c WHERE id = ?`, id).
		Scan(&code, &title, &desc, &subs)
	if err != nil {
		a.notFound(w, r)
		return
	}
	users, err := a.activeUsers(id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.page(w, r, "admin_control", View{Title: "Edit " + code, Nav: "controls", D: map[string]any{
		"ID": id, "Code": code, "ControlTitle": title, "Description": desc, "Users": users, "Subs": subs,
	}})
}

func (a *App) saveControl(w http.ResponseWriter, r *http.Request) {
	id := pathID(r) // 0 when creating
	back := "/admin/controls"
	if id != 0 {
		back = fmt.Sprintf("/admin/controls/%d", id)
	}
	code := strings.TrimSpace(r.PostForm.Get("code"))
	title := strings.TrimSpace(r.PostForm.Get("title"))
	desc := strings.TrimSpace(r.PostForm.Get("description"))
	if code == "" || title == "" || len(code) > 40 || len(title) > 200 || len(desc) > 4000 {
		a.redirect(w, r, back, flashErr("Give the control a code (up to 40 characters) and a title (up to 200)."))
		return
	}
	err := func() error {
		tx, err := a.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if id == 0 {
			res, err := tx.Exec(`INSERT INTO controls (code, title, description, created_at) VALUES (?, ?, ?, ?)`, code, title, desc, a.now().Unix())
			if err != nil {
				return err
			}
			id, _ = res.LastInsertId()
		} else {
			res, err := tx.Exec(`UPDATE controls SET code = ?, title = ?, description = ? WHERE id = ?`, code, title, desc, id)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return sql.ErrNoRows
			}
		}
		if _, err := tx.Exec(`DELETE FROM control_owners WHERE control_id = ?`, id); err != nil {
			return err
		}
		for _, o := range r.PostForm["owners"] {
			uid, _ := strconv.ParseInt(o, 10, 64)
			if _, err := tx.Exec(`INSERT OR IGNORE INTO control_owners (control_id, user_id) SELECT ?, id FROM users WHERE id = ? AND active = 1`, id, uid); err != nil {
				return err
			}
		}
		return tx.Commit()
	}()
	switch {
	case err != nil && strings.Contains(err.Error(), "UNIQUE"):
		a.redirect(w, r, back, flashErr(fmt.Sprintf("A control with the code %s already exists.", code)))
	case errors.Is(err, sql.ErrNoRows):
		a.notFound(w, r)
	case err != nil:
		a.fail(w, r, err)
	default:
		a.redirect(w, r, "/admin/controls", fmt.Sprintf("Control %s saved.", code))
	}
}

// storedFiles lists the on disk names to unlink after a delete commits.
func (a *App) storedFiles(query string, id int64) []string {
	rows, err := a.db.Query(query, id)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func (a *App) unlink(stored []string) {
	for _, s := range stored {
		if err := os.Remove(filepath.Join(a.cfg.DataDir, "files", s)); err != nil && !os.IsNotExist(err) {
			log.Printf("remove %s: %v", s, err)
		}
	}
}

func (a *App) deleteControl(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	stored := a.storedFiles(`SELECT f.stored_name FROM files f JOIN submissions s ON s.id = f.submission_id WHERE s.control_id = ?`, id)
	if _, err := a.db.Exec(`DELETE FROM controls WHERE id = ?`, id); err != nil {
		a.fail(w, r, err)
		return
	}
	a.unlink(stored)
	a.redirect(w, r, "/admin/controls", "Control deleted with its evidence.")
}

func (a *App) deleteSubmission(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	var controlID int64
	if err := a.db.QueryRow(`SELECT control_id FROM submissions WHERE id = ?`, id).Scan(&controlID); err != nil {
		a.notFound(w, r)
		return
	}
	stored := a.storedFiles(`SELECT stored_name FROM files WHERE submission_id = ?`, id)
	if _, err := a.db.Exec(`DELETE FROM submissions WHERE id = ?`, id); err != nil {
		a.fail(w, r, err)
		return
	}
	a.unlink(stored)
	a.redirect(w, r, fmt.Sprintf("/controls/%d", controlID), "Submission removed.")
}

// ---------- admin: users ----------

type UserRow struct {
	User
	Controls int
}

func (a *App) adminUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT u.id, u.email, u.name, u.role, u.active, u.must_change,
		(SELECT COUNT(*) FROM control_owners o WHERE o.user_id = u.id) FROM users u ORDER BY u.active DESC, u.name`)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	defer rows.Close()
	var list []UserRow
	for rows.Next() {
		var u UserRow
		rows.Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.Active, &u.MustChange, &u.Controls)
		list = append(list, u)
	}
	a.page(w, r, "admin_users", View{Title: "People", Nav: "users", D: map[string]any{"Rows": list}})
}

func (a *App) createUser(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostForm.Get("name"))
	email := strings.TrimSpace(r.PostForm.Get("email"))
	role := r.PostForm.Get("role")
	if name == "" || len(name) > 120 || !validEmail(email) || (role != "admin" && role != "user") {
		a.redirect(w, r, "/admin/users", flashErr("Give a name, a valid email and a role."))
		return
	}
	pass := tempPassword()
	hash, err := bcrypt.GenerateFromPassword([]byte(pass), 12)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	_, err = a.db.Exec(`INSERT INTO users (email, name, role, password_hash, must_change, created_at) VALUES (?, ?, ?, ?, 1, ?)`,
		email, name, role, string(hash), a.now().Unix())
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		a.redirect(w, r, "/admin/users", flashErr(email+" already has an account."))
		return
	} else if err != nil {
		a.fail(w, r, err)
		return
	}
	a.redirect(w, r, "/admin/users", flashSticky(fmt.Sprintf("%s can now sign in as %s with the temporary password %s. They will choose their own password on first sign in. This password is shown only once.", name, email, pass)))
}

func (a *App) editUser(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	var u User
	err := a.db.QueryRow(`SELECT id, email, name, role, active, must_change FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.Active, &u.MustChange)
	if err != nil {
		a.notFound(w, r)
		return
	}
	rows, err := a.db.Query(`SELECT c.id, c.code, c.title FROM controls c JOIN control_owners o ON o.control_id = c.id WHERE o.user_id = ? ORDER BY c.code`, id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	defer rows.Close()
	var owned []ControlRow
	for rows.Next() {
		var c ControlRow
		rows.Scan(&c.ID, &c.Code, &c.Title)
		owned = append(owned, c)
	}
	a.page(w, r, "admin_user", View{Title: u.Name, Nav: "users", D: map[string]any{"U": u, "Owned": owned, "Self": id == currentUser(r).ID}})
}

func (a *App) activeAdmins() int {
	var n int
	a.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = 'admin' AND active = 1`).Scan(&n)
	return n
}

func (a *App) updateUser(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	back := fmt.Sprintf("/admin/users/%d", id)
	var old User
	if err := a.db.QueryRow(`SELECT role, active FROM users WHERE id = ?`, id).Scan(&old.Role, &old.Active); err != nil {
		a.notFound(w, r)
		return
	}
	name := strings.TrimSpace(r.PostForm.Get("name"))
	role := r.PostForm.Get("role")
	active := r.PostForm.Get("active") == "1"
	if name == "" || len(name) > 120 || (role != "admin" && role != "user") {
		a.redirect(w, r, back, flashErr("Give a name and a role."))
		return
	}
	losesAdmin := old.Role == "admin" && old.Active && (role != "admin" || !active)
	if losesAdmin && id == currentUser(r).ID {
		a.redirect(w, r, back, flashErr("You cannot remove your own admin access. Ask another admin."))
		return
	}
	if losesAdmin && a.activeAdmins() <= 1 {
		a.redirect(w, r, back, flashErr("Keep at least one active admin."))
		return
	}
	if _, err := a.db.Exec(`UPDATE users SET name = ?, role = ?, active = ? WHERE id = ?`, name, role, active, id); err != nil {
		a.fail(w, r, err)
		return
	}
	if role != old.Role || active != old.Active {
		a.db.Exec(`DELETE FROM sessions WHERE user_id = ?`, id)
	}
	a.redirect(w, r, "/admin/users", "Saved "+name+".")
}

func (a *App) resetUser(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	pass := tempPassword()
	hash, err := bcrypt.GenerateFromPassword([]byte(pass), 12)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var name string
	if err := a.db.QueryRow(`UPDATE users SET password_hash = ?, must_change = 1 WHERE id = ? RETURNING name`, string(hash), id).Scan(&name); err != nil {
		a.notFound(w, r)
		return
	}
	a.db.Exec(`DELETE FROM sessions WHERE user_id = ?`, id)
	a.redirect(w, r, fmt.Sprintf("/admin/users/%d", id), flashSticky(fmt.Sprintf("New temporary password for %s: %s. It is shown only once.", name, pass)))
}

// ---------- admin: settings ----------

func (a *App) settingsPage(w http.ResponseWriter, r *http.Request) {
	s := a.settings()
	a.page(w, r, "admin_settings", View{Title: "Settings", Nav: "settings", D: map[string]any{"S": s, "HasPass": s.SMTPPass != ""}})
}

func (a *App) saveSettings(w http.ResponseWriter, r *http.Request) {
	f := r.PostForm
	kick, err1 := strconv.Atoi(f.Get("kickoff_day"))
	rem, err2 := strconv.Atoi(f.Get("reminder_days"))
	port, err3 := strconv.Atoi(f.Get("smtp_port"))
	tlsMode := f.Get("smtp_tls")
	switch {
	case err1 != nil || kick < 1 || kick > 7:
		a.redirect(w, r, "/admin/settings", flashErr("The kickoff day must be between 1 and 7."))
		return
	case err2 != nil || rem < 0 || rem > 31:
		a.redirect(w, r, "/admin/settings", flashErr("Remind every 0 to 31 days. Use 0 to turn reminders off."))
		return
	case f.Get("smtp_host") != "" && (err3 != nil || port < 1 || port > 65535):
		a.redirect(w, r, "/admin/settings", flashErr("Give a valid SMTP port, usually 587."))
		return
	case tlsMode != "starttls" && tlsMode != "tls" && tlsMode != "none":
		a.redirect(w, r, "/admin/settings", flashErr("Choose how to secure the mail connection."))
		return
	case f.Get("smtp_from") != "" && !validEmail(strings.TrimSpace(f.Get("smtp_from"))):
		a.redirect(w, r, "/admin/settings", flashErr("The sender must be a plain email address."))
		return
	}
	vals := map[string]string{
		"smtp_host": strings.TrimSpace(f.Get("smtp_host")), "smtp_port": strconv.Itoa(port),
		"smtp_user": strings.TrimSpace(f.Get("smtp_user")), "smtp_from": strings.TrimSpace(f.Get("smtp_from")),
		"smtp_tls": tlsMode, "base_url": strings.TrimRight(strings.TrimSpace(f.Get("base_url")), "/"),
		"kickoff_day": strconv.Itoa(kick), "reminder_days": strconv.Itoa(rem),
	}
	if p := f.Get("smtp_pass"); p != "" {
		vals["smtp_pass"] = p
	}
	if f.Get("clear_pass") == "1" {
		vals["smtp_pass"] = ""
	}
	tx, err := a.db.Begin()
	if err != nil {
		a.fail(w, r, err)
		return
	}
	defer tx.Rollback()
	for k, v := range vals {
		if _, err := tx.Exec(`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			a.fail(w, r, err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		a.fail(w, r, err)
		return
	}
	a.redirect(w, r, "/admin/settings", "Settings saved.")
}

func (a *App) testEmail(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	s := a.settings()
	if s.SMTPHost == "" {
		a.redirect(w, r, "/admin/settings", flashErr("Add an SMTP server first."))
		return
	}
	body := "This is a test message from BrewDat Evidence Collection.\n\nIf you can read this, monthly requests and reminders will reach your owners.\n"
	if err := a.mailer(s, u.Email, "BrewDat Evidence Collection test message", body); err != nil {
		log.Printf("test email: %v", err)
		a.redirect(w, r, "/admin/settings", flashErr("The mail server refused the message: "+strings.ReplaceAll(err.Error(), "-", " ")))
		return
	}
	a.redirect(w, r, "/admin/settings", "Test message sent to "+u.Email+".")
}

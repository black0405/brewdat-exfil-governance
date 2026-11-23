// BrewDat Evidence Collection: a single binary evidence portal.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

//go:embed web
var webFS embed.FS

const schema = `
CREATE TABLE IF NOT EXISTS users (
  id INTEGER PRIMARY KEY,
  email TEXT NOT NULL UNIQUE COLLATE NOCASE,
  name TEXT NOT NULL,
  role TEXT NOT NULL CHECK (role IN ('admin','user')),
  password_hash TEXT NOT NULL,
  must_change INTEGER NOT NULL DEFAULT 0,
  active INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
  token_hash TEXT PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  csrf TEXT NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS controls (
  id INTEGER PRIMARY KEY,
  code TEXT NOT NULL UNIQUE COLLATE NOCASE,
  title TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS control_owners (
  control_id INTEGER NOT NULL REFERENCES controls(id) ON DELETE CASCADE,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  PRIMARY KEY (control_id, user_id)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS submissions (
  id INTEGER PRIMARY KEY,
  control_id INTEGER NOT NULL REFERENCES controls(id) ON DELETE CASCADE,
  period INTEGER NOT NULL,
  user_id INTEGER NOT NULL REFERENCES users(id),
  note TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS submissions_cp ON submissions(control_id, period);
CREATE TABLE IF NOT EXISTS files (
  id INTEGER PRIMARY KEY,
  submission_id INTEGER NOT NULL REFERENCES submissions(id) ON DELETE CASCADE,
  orig_name TEXT NOT NULL,
  stored_name TEXT NOT NULL UNIQUE,
  size INTEGER NOT NULL,
  sha256 TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS email_log (
  id INTEGER PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  period INTEGER NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('kickoff','reminder')),
  sent_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS email_kickoff_once ON email_log(user_id, period) WHERE kind = 'kickoff';
CREATE INDEX IF NOT EXISTS email_log_up ON email_log(user_id, period);
`

type Config struct {
	Addr, DataDir, TLSCert, TLSKey string
	MaxUploadMB                    int64
	Secure, BehindProxy            bool
}

type User struct {
	ID          int64
	Email, Name string
	Role        string
	Active      bool
	MustChange  bool
}

func (u *User) IsAdmin() bool { return u != nil && u.Role == "admin" }

type App struct {
	cfg     Config
	db      *sql.DB
	tpl     *template.Template
	lim     *limiter
	now     func() time.Time
	tickMu  sync.Mutex
	mailer  func(s Settings, to, subject, body string) error
	lastBak string
}

type ctxKey int

const (
	keyUser ctxKey = iota
	keyCSRF
)

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func loadConfig() Config {
	mb, _ := strconv.ParseInt(env("BREWDAT_MAX_UPLOAD_MB", "25"), 10, 64)
	c := Config{
		Addr:        env("BREWDAT_ADDR", ":8080"),
		DataDir:     env("BREWDAT_DATA", "./data"),
		TLSCert:     env("BREWDAT_TLS_CERT", ""),
		TLSKey:      env("BREWDAT_TLS_KEY", ""),
		MaxUploadMB: max(mb, 1),
		BehindProxy: env("BREWDAT_BEHIND_PROXY", "0") == "1",
	}
	c.Secure = c.TLSCert != "" || c.BehindProxy
	return c
}

func openDB(dataDir string) (*sql.DB, error) {
	for _, d := range []string{"files", "backup"} {
		if err := os.MkdirAll(filepath.Join(dataDir, d), 0o750); err != nil {
			return nil, err
		}
	}
	dsn := "file:" + filepath.Join(dataDir, "brewdat.db") +
		"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	return db, nil
}

func newApp(cfg Config, db *sql.DB) (*App, error) {
	// Asset URLs carry a hash of the embedded static files, so browsers fetch fresh CSS and JS after every deploy.
	sum := sha256.New()
	fs.WalkDir(webFS, "web/static", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := webFS.ReadFile(p)
			sum.Write(b)
		}
		return nil
	})
	version := hex.EncodeToString(sum.Sum(nil))[:10]
	tpl, err := template.New("").Funcs(funcs).Funcs(template.FuncMap{"v": func() string { return version }}).ParseFS(webFS, "web/templates/*.html")
	if err != nil {
		return nil, err
	}
	return &App{cfg: cfg, db: db, tpl: tpl, lim: &limiter{hits: map[string]*hit{}}, now: time.Now, mailer: sendMail}, nil
}

// setAdmin creates an admin or resets an existing user to admin with a fresh temporary password.
func (a *App) setAdmin(email string) (string, error) {
	email = strings.TrimSpace(email)
	if !validEmail(email) {
		return "", errors.New("give a valid email address")
	}
	pass := tempPassword()
	hash, err := bcrypt.GenerateFromPassword([]byte(pass), 12)
	if err != nil {
		return "", err
	}
	_, err = a.db.Exec(`INSERT INTO users (email, name, role, password_hash, must_change, created_at)
		VALUES (?, 'Administrator', 'admin', ?, 1, ?)
		ON CONFLICT(email) DO UPDATE SET role = 'admin', active = 1, must_change = 1, password_hash = excluded.password_hash`,
		email, string(hash), a.now().Unix())
	if err == nil {
		a.db.Exec(`DELETE FROM sessions WHERE user_id = (SELECT id FROM users WHERE email = ?)`, email)
	}
	return pass, err
}

func main() {
	cfg := loadConfig()
	db, err := openDB(cfg.DataDir)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	app, err := newApp(cfg, db)
	if err != nil {
		log.Fatal(err)
	}

	// CLI: brewdat admin EMAIL creates the first admin or resets one (break glass recovery).
	if len(os.Args) == 3 && os.Args[1] == "admin" {
		pass, err := app.setAdmin(os.Args[2])
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("Admin %s is ready. Temporary password: %s\n", os.Args[2], pass)
		return
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	if n == 0 {
		log.Print("no users yet; create the first admin with: brewdat admin you@company.com")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go app.scheduler(ctx)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Minute, // large uploads on slow links
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	log.Printf("BrewDat Evidence Collection listening on %s", cfg.Addr)
	if cfg.TLSCert != "" {
		err = srv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
	} else {
		err = srv.ListenAndServe()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// ---------- routing and middleware ----------

func (a *App) routes() http.Handler {
	static, _ := fs.Sub(webFS, "web/static")
	files := http.StripPrefix("/static/", http.FileServerFS(static))
	m := http.NewServeMux()
	m.HandleFunc("GET /static/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		files.ServeHTTP(w, r)
	})
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	m.HandleFunc("GET /login", a.loginPage)
	m.HandleFunc("POST /login", a.loginSubmit)

	user := func(p string, h http.HandlerFunc) { m.Handle(p, a.auth(h, false)) }
	admin := func(p string, h http.HandlerFunc) { m.Handle(p, a.auth(h, true)) }

	user("POST /logout", a.logout)
	user("GET /{$}", a.dashboard)
	user("GET /controls/{id}", a.controlPage)
	user("POST /controls/{id}/evidence", a.uploadEvidence)
	user("GET /files/{id}", a.downloadFile)
	user("GET /account", a.accountPage)
	user("POST /account/password", a.changePassword)

	admin("GET /admin/controls", a.adminControls)
	admin("POST /admin/controls", a.saveControl)
	admin("GET /admin/controls/{id}", a.editControl)
	admin("POST /admin/controls/{id}", a.saveControl)
	admin("POST /admin/controls/{id}/delete", a.deleteControl)
	admin("POST /admin/submissions/{id}/delete", a.deleteSubmission)
	admin("GET /admin/users", a.adminUsers)
	admin("POST /admin/users", a.createUser)
	admin("GET /admin/users/{id}", a.editUser)
	admin("POST /admin/users/{id}", a.updateUser)
	admin("POST /admin/users/{id}/reset", a.resetUser)
	admin("GET /admin/settings", a.settingsPage)
	admin("POST /admin/settings", a.saveSettings)
	admin("POST /admin/settings/test", a.testEmail)

	return a.headers(m)
}

func (a *App) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com; img-src 'self' data:; frame-ancestors 'none'; form-action 'self'; base-uri 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		if a.cfg.Secure {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// auth loads the session user, enforces the role, limits and parses POST bodies and checks CSRF.
func (a *App) auth(next http.HandlerFunc, adminOnly bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, csrf, ok := a.sessionUser(r)
		if !ok {
			a.clearCookie(w, "session")
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		r = r.WithContext(context.WithValue(context.WithValue(r.Context(), keyUser, u), keyCSRF, csrf))
		if adminOnly && !u.IsAdmin() {
			a.errorPage(w, r, http.StatusForbidden, "This page is for administrators.")
			return
		}
		if u.MustChange && r.URL.Path != "/account" && r.URL.Path != "/account/password" && r.URL.Path != "/logout" {
			http.Redirect(w, r, "/account", http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost {
			limit := int64(1 << 20)
			multipart := strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/")
			if multipart {
				limit += a.cfg.MaxUploadMB << 20 * maxFiles
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			var err error
			if multipart {
				err = r.ParseMultipartForm(1 << 20)
				if r.MultipartForm != nil {
					defer r.MultipartForm.RemoveAll()
				}
			} else {
				err = r.ParseForm()
			}
			if err != nil {
				var tooBig *http.MaxBytesError
				if errors.As(err, &tooBig) {
					a.errorPage(w, r, http.StatusRequestEntityTooLarge, fmt.Sprintf("That upload is too large. Each file can be up to %d MB.", a.cfg.MaxUploadMB))
					return
				}
				a.errorPage(w, r, http.StatusBadRequest, "The form could not be read. Try again.")
				return
			}
			if subtle.ConstantTimeCompare([]byte(r.PostForm.Get("csrf")), []byte(csrf)) != 1 {
				a.errorPage(w, r, http.StatusForbidden, "This form has expired. Reload the page and try again.")
				return
			}
		}
		next(w, r)
	})
}

func (a *App) sessionUser(r *http.Request) (*User, string, bool) {
	c, err := r.Cookie("session")
	if err != nil {
		return nil, "", false
	}
	var u User
	var csrf string
	err = a.db.QueryRow(`SELECT u.id, u.email, u.name, u.role, u.active, u.must_change, s.csrf
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND s.expires_at > ? AND u.active = 1`, hashToken(c.Value), a.now().Unix()).
		Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.Active, &u.MustChange, &csrf)
	return &u, csrf, err == nil
}

func currentUser(r *http.Request) *User { u, _ := r.Context().Value(keyUser).(*User); return u }

// ---------- rendering ----------

type View struct {
	Title  string
	Nav    string
	User   *User
	CSRF   string
	Flash  string // toast
	Notice string // banner that stays, used for temporary passwords
	Err    string // error banner
	D      map[string]any
}

var funcs = template.FuncMap{
	"date": func(unix int64) string {
		if unix == 0 {
			return "Never"
		}
		return time.Unix(unix, 0).Format("2 Jan 2006 at 15:04")
	},
	"period": periodLabel,
	"size": func(n int64) string {
		switch {
		case n >= 1<<20:
			return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
		case n >= 1<<10:
			return fmt.Sprintf("%d KB", n/(1<<10))
		}
		return fmt.Sprintf("%d bytes", n)
	},
	"ratio": func(done, total int) string {
		if total == 0 {
			return "0"
		}
		return strconv.FormatFloat(float64(done)/float64(total), 'f', 3, 64)
	},
	"percent": func(done, total int) int {
		if total == 0 {
			return 0
		}
		return done * 100 / total
	},
}

func (a *App) render(w http.ResponseWriter, r *http.Request, status int, page string, v View) {
	v.User = currentUser(r)
	v.CSRF, _ = r.Context().Value(keyCSRF).(string)
	if c, err := r.Cookie("flash"); err == nil {
		msg, _ := url.QueryUnescape(c.Value)
		switch {
		case strings.HasPrefix(msg, "!"):
			v.Err = msg[1:]
		case strings.HasPrefix(msg, "*"):
			v.Notice = msg[1:]
		default:
			v.Flash = msg
		}
		a.clearCookie(w, "flash")
	}
	if v.D == nil {
		v.D = map[string]any{}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := a.tpl.ExecuteTemplate(w, page, v); err != nil {
		log.Printf("render %s: %v", page, err)
	}
}

func (a *App) page(w http.ResponseWriter, r *http.Request, page string, v View) {
	a.render(w, r, http.StatusOK, page, v)
}

func (a *App) errorPage(w http.ResponseWriter, r *http.Request, status int, msg string) {
	a.render(w, r, status, "error", View{Title: http.StatusText(status), D: map[string]any{"Msg": msg}})
}

func (a *App) notFound(w http.ResponseWriter, r *http.Request) {
	a.errorPage(w, r, http.StatusNotFound, "That page does not exist or you do not have access to it.")
}

func (a *App) redirect(w http.ResponseWriter, r *http.Request, to, flash string) {
	if flash != "" {
		http.SetCookie(w, &http.Cookie{Name: "flash", Value: url.QueryEscape(flash), Path: "/", HttpOnly: true, Secure: a.cfg.Secure, SameSite: http.SameSiteLaxMode})
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func (a *App) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.cfg.Secure})
}

func (a *App) fail(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("error %s %s: %v", r.Method, r.URL.Path, err)
	a.errorPage(w, r, http.StatusInternalServerError, "Something went wrong on the server. The error was logged.")
}

// ---------- small helpers ----------

func token(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func hashToken(t string) string {
	s := sha256.Sum256([]byte(t))
	return hex.EncodeToString(s[:])
}

// tempPassword avoids look alike characters so it can be read out over the phone.
func tempPassword() string {
	const alphabet = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKMNPQRSTUVWXYZ23456789"
	b := make([]byte, 14)
	rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

func (a *App) clientIP(r *http.Request) string {
	if a.cfg.BehindProxy {
		if f := r.Header.Get("X-Forwarded-For"); f != "" {
			return strings.TrimSpace(strings.Split(f, ",")[0])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Periods are stored as yyyymm integers (202611) and shown as "November 2026".
func periodOf(t time.Time) int { return t.Year()*100 + int(t.Month()) }

func periodTime(p int) time.Time {
	return time.Date(p/100, time.Month(p%100), 1, 0, 0, 0, 0, time.Local)
}

func periodLabel(p int) string { return periodTime(p).Format("January 2006") }

// recentPeriods returns the current period and the n-1 before it, newest first.
func recentPeriods(now time.Time, n int) []int {
	out := make([]int, n)
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local)
	for i := range out {
		out[i] = periodOf(first.AddDate(0, -i, 0))
	}
	return out
}

// limiter blocks an IP after 10 failed sign ins within 15 minutes.
// ponytail: in memory, per process; move it to the DB if you ever run more than one instance.
type hit struct {
	n     int
	first time.Time
}

type limiter struct {
	mu   sync.Mutex
	hits map[string]*hit
}

func (l *limiter) blocked(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	h := l.hits[ip]
	if h != nil && now.Sub(h.first) > 15*time.Minute {
		delete(l.hits, ip)
		return false
	}
	return h != nil && h.n >= 10
}

func (l *limiter) fail(ip string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if h := l.hits[ip]; h != nil {
		h.n++
		return
	}
	l.hits[ip] = &hit{n: 1, first: now}
}

func (l *limiter) reset(ip string) {
	l.mu.Lock()
	delete(l.hits, ip)
	l.mu.Unlock()
}

func (l *limiter) prune(now time.Time) {
	l.mu.Lock()
	for ip, h := range l.hits {
		if now.Sub(h.first) > 15*time.Minute {
			delete(l.hits, ip)
		}
	}
	l.mu.Unlock()
}

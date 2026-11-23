package main

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDue(t *testing.T) {
	at := func(day, hour int) time.Time { return time.Date(2026, 3, day, hour, 0, 0, 0, time.Local) }
	never := time.Time{}
	cases := []struct {
		name        string
		now         time.Time
		kickoffDay  int
		remind      int
		kickoffSent bool
		last        time.Time
		open        int
		want        string
	}{
		{"before kickoff day", at(1, 9), 3, 7, false, never, 2, ""},
		{"kickoff day too early in the morning", at(3, 7), 3, 7, false, never, 2, ""},
		{"kickoff day after send hour", at(3, 8), 3, 7, false, never, 2, "kickoff"},
		{"late kickoff after downtime", at(20, 9), 1, 7, false, never, 2, "kickoff"},
		{"nothing open", at(3, 9), 1, 7, false, never, 0, ""},
		{"kickoff already sent", at(3, 9), 1, 7, true, at(3, 8), 2, ""},
		{"interval not reached", at(9, 9), 1, 7, true, at(3, 8), 2, ""},
		{"interval reached", at(10, 9), 1, 7, true, at(3, 8), 2, "reminder"},
		{"daily reminder next morning", at(4, 8), 1, 1, true, at(3, 23), 1, "reminder"},
		{"reminders off", at(28, 9), 1, 0, true, at(3, 8), 2, ""},
	}
	for _, c := range cases {
		if got := due(c.now, c.kickoffDay, c.remind, c.kickoffSent, c.last, c.open); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// ---------- end to end ----------

type client struct {
	t    *testing.T
	base string
	c    *http.Client
}

func newClient(t *testing.T, base string) *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: t, base: base, c: &http.Client{Jar: jar}}
}

func (c *client) get(path string) (int, string) {
	res, err := c.c.Get(c.base + path)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`)

func (c *client) csrf(path string) string {
	_, body := c.get(path)
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		c.t.Fatalf("no csrf token on %s", path)
	}
	return m[1]
}

func (c *client) post(path string, form url.Values) (int, string) {
	res, err := c.c.PostForm(c.base+path, form)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func (c *client) login(email, pass string) {
	code, body := c.post("/login", url.Values{"email": {email}, "password": {pass}})
	if code != 200 || !strings.Contains(body, "Sign out") {
		c.t.Fatalf("login %s failed: %d", email, code)
	}
}

// visibleText strips scripts, styles and tags, keeping attributes people can read.
var (
	blockRe = regexp.MustCompile(`(?s)<(script|style)[^>]*>.*?</(script|style)>`)
	attrRe  = regexp.MustCompile(`(?:placeholder|aria-label|title|data-toast|data-confirm|data-busy)="([^"]*)"`)
	tagRe   = regexp.MustCompile(`(?s)<[^>]*>`)
)

func visibleText(html string) string {
	html = blockRe.ReplaceAllString(html, " ")
	var attrs []string
	for _, m := range attrRe.FindAllStringSubmatch(html, -1) {
		attrs = append(attrs, m[1])
	}
	return tagRe.ReplaceAllString(html, " ") + " " + strings.Join(attrs, " ")
}

func assertNoDash(t *testing.T, where, html string) {
	t.Helper()
	text := visibleText(html)
	for _, d := range []string{"-", "–", "—"} {
		if i := strings.Index(text, d); i >= 0 {
			lo, hi := max(0, i-40), min(len(text), i+40)
			t.Errorf("%s shows %q near: %q", where, d, text[lo:hi])
		}
	}
}

func TestPortal(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	app, err := newApp(Config{DataDir: dir, MaxUploadMB: 5}, db)
	if err != nil {
		t.Fatal(err)
	}
	var sent []string
	app.mailer = func(s Settings, to, subject, body string) error {
		sent = append(sent, to+"|"+subject+"|"+body)
		return nil
	}
	srv := httptest.NewServer(app.routes())
	defer srv.Close()

	// Sign in page
	anon := newClient(t, srv.URL)
	_, body := anon.get("/login")
	assertNoDash(t, "login", body)

	// First admin must change the temporary password before anything else.
	temp, err := app.setAdmin("admin@brewdat.local")
	if err != nil {
		t.Fatal(err)
	}
	admin := newClient(t, srv.URL)
	admin.login("admin@brewdat.local", temp)
	tok := admin.csrf("/account")
	if code, _ := admin.get("/admin/users"); code != 200 {
		t.Fatalf("expected redirect to account page, got %d", code)
	}
	if code, body := admin.post("/account/password", url.Values{"csrf": {tok}, "current": {temp}, "new": {"golden lager 2026"}, "confirm": {"golden lager 2026"}}); code != 200 || !strings.Contains(body, "Evidence overview") {
		t.Fatalf("password change failed: %d", code)
	}
	tok = admin.csrf("/")

	// Admin creates two people and a control owned by the first.
	for _, p := range [][2]string{{"Alex Owner", "alex@brewdat.local"}, {"Sam Other", "sam@brewdat.local"}} {
		admin.post("/admin/users", url.Values{"csrf": {tok}, "name": {p[0]}, "email": {p[1]}, "role": {"user"}})
	}
	var alexID int64
	db.QueryRow(`SELECT id FROM users WHERE email = 'alex@brewdat.local'`).Scan(&alexID)
	db.Exec(`UPDATE users SET must_change = 0, password_hash = (SELECT password_hash FROM users WHERE email = 'admin@brewdat.local') WHERE role = 'user'`)
	admin.post("/admin/controls", url.Values{"csrf": {tok}, "code": {"AC 01"}, "title": {"Access review"}, "description": {"Export of the reviewed access list"}, "owners": {itoa(alexID)}})
	var controlID int64
	if err := db.QueryRow(`SELECT id FROM controls WHERE code = 'AC 01'`).Scan(&controlID); err != nil {
		t.Fatal("control not created")
	}
	cpath := "/controls/" + itoa(controlID)

	// Owner uploads evidence.
	alex := newClient(t, srv.URL)
	alex.login("alex@brewdat.local", "golden lager 2026")
	atok := alex.csrf(cpath)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("csrf", atok)
	mw.WriteField("period", itoa(int64(periodOf(time.Now()))))
	mw.WriteField("note", "Reviewed by the team")
	fw, _ := mw.CreateFormFile("files", "access_list.pdf")
	fw.Write([]byte("%PDF fake evidence"))
	mw.Close()
	res, err := alex.c.Post(srv.URL+cpath+"/evidence", mw.FormDataContentType(), &buf)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("upload failed: %v %v", err, res.StatusCode)
	}
	res.Body.Close()
	var fileID int64
	if err := db.QueryRow(`SELECT id FROM files`).Scan(&fileID); err != nil {
		t.Fatal("file row missing")
	}
	if code, body := alex.get("/files/" + itoa(fileID)); code != 200 || body != "%PDF fake evidence" {
		t.Fatalf("download failed: %d %q", code, body)
	}

	res, err = alex.c.Get(srv.URL + "/files/" + itoa(fileID) + "?view=1")
	if err != nil || res.Header.Get("Content-Type") != "application/pdf" || !strings.HasPrefix(res.Header.Get("Content-Disposition"), "inline") {
		t.Fatalf("preview headers wrong: %v %v", err, res.Header)
	}
	res.Body.Close()

	// Every page, as each role, is free of dashes in visible text.
	var subID int64
	db.QueryRow(`SELECT id FROM submissions`).Scan(&subID)
	for _, p := range []string{"/", cpath, "/account", "/admin/controls", "/admin/controls/" + itoa(controlID), "/admin/users", "/admin/users/" + itoa(alexID), "/admin/settings", "/nope"} {
		_, body := admin.get(p)
		assertNoDash(t, "admin "+p, body)
	}
	for _, p := range []string{"/", cpath, "/account", "/admin/users"} {
		_, body := alex.get(p)
		assertNoDash(t, "user "+p, body)
	}

	// Authorisation.
	sam := newClient(t, srv.URL)
	sam.login("sam@brewdat.local", "golden lager 2026")
	if code, _ := sam.get(cpath); code != 404 {
		t.Errorf("non owner control page: got %d, want 404", code)
	}
	if code, _ := sam.get("/files/" + itoa(fileID) + "?view=1"); code != 404 {
		t.Errorf("non owner preview: got %d, want 404", code)
	}
	if code, _ := sam.get("/files/" + itoa(fileID)); code != 404 {
		t.Errorf("non owner file: got %d, want 404", code)
	}
	if code, _ := alex.get("/admin/controls"); code != 403 {
		t.Errorf("user on admin page: got %d, want 403", code)
	}
	if code, _ := admin.post("/admin/users", url.Values{"name": {"X"}, "email": {"x@brewdat.local"}, "role": {"user"}}); code != 403 {
		t.Errorf("post without csrf: got %d, want 403", code)
	}

	// Scheduler: kickoff once, reminder after the interval, silence once evidence is in.
	db.Exec(`INSERT INTO settings (key, value) VALUES ('smtp_host', 'mail.test'), ('kickoff_day', '1'), ('reminder_days', '7')`)
	march := func(day int) time.Time { return time.Date(2026, 3, day, 9, 0, 0, 0, time.Local) }
	app.tick(march(1))
	app.tick(march(2))
	if len(sent) != 1 || !strings.Contains(sent[0], "Evidence requested for March 2026") {
		t.Fatalf("want one kickoff, got %d: %v", len(sent), sent)
	}
	app.tick(march(8))
	if len(sent) != 2 || !strings.Contains(sent[1], "Reminder") {
		t.Fatalf("want a reminder on day 8, got %v", sent)
	}
	db.Exec(`INSERT INTO submissions (control_id, period, user_id, created_at) VALUES (?, 202603, ?, 0)`, controlID, alexID)
	app.tick(march(20))
	if len(sent) != 2 {
		t.Fatalf("no email expected once evidence is in, got %v", sent)
	}
	for _, m := range sent {
		assertNoDash(t, "email", m)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

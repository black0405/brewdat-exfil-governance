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
		name            string
		now             time.Time
		kickoffDay      int
		remind          int
		kickoffSent     bool
		last            time.Time
		missing, denied int
		want            string
	}{
		{"before kickoff day", at(1, 9), 3, 7, false, never, 2, 0, ""},
		{"kickoff day too early in the morning", at(3, 7), 3, 7, false, never, 2, 0, ""},
		{"kickoff day after send hour", at(3, 8), 3, 7, false, never, 2, 0, "kickoff"},
		{"late kickoff after downtime", at(20, 9), 1, 7, false, never, 2, 0, "kickoff"},
		{"nothing open", at(3, 9), 1, 7, false, never, 0, 0, ""},
		{"kickoff already sent", at(3, 9), 1, 7, true, at(3, 8), 2, 0, ""},
		{"interval not reached", at(9, 9), 1, 7, true, at(3, 8), 2, 0, ""},
		{"interval reached", at(10, 9), 1, 7, true, at(3, 8), 2, 0, "reminder"},
		{"daily reminder next morning", at(4, 8), 1, 1, true, at(3, 23), 1, 0, "reminder"},
		{"reminders off", at(28, 9), 1, 0, true, at(3, 8), 2, 0, ""},
		{"submitted, awaiting review", at(10, 9), 1, 7, true, at(3, 8), 0, 0, ""},
		{"denied, interval reached", at(10, 9), 1, 7, true, at(3, 8), 0, 1, "reminder"},
		{"denied yesterday, weekly", at(10, 9), 1, 7, true, at(9, 9), 0, 1, ""},
		{"denied only, no kickoff, interval reached", at(10, 9), 1, 7, false, at(3, 8), 0, 1, "reminder"},
		{"denied only, denied today", at(10, 9), 1, 7, false, at(10, 8), 0, 1, ""},
		{"missing and denied, before kickoff day", at(2, 9), 3, 7, false, never, 1, 1, ""},
		{"missing and denied, kickoff day", at(3, 9), 3, 7, false, never, 1, 1, "kickoff"},
	}
	for _, c := range cases {
		if got := due(c.now, c.kickoffDay, c.remind, c.kickoffSent, c.last, c.missing, c.denied); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestFigures(t *testing.T) {
	for in, want := range map[string]string{"1": "B01", "7.0": "B07", "b3": "B03", "B012": "B12", "B105": "B105", "AC 01": "AC 01"} {
		if got := controlCode(in); got != want {
			t.Errorf("controlCode(%q) = %q, want %q", in, got, want)
		}
	}
	if msg := checkFigures(100, 40, 10); msg != "" {
		t.Errorf("valid figures refused: %s", msg)
	}
	if msg := checkFigures(100, 60, 50); msg == "" {
		t.Error("remediated plus accepted above identified was accepted")
	}
	if s, ok := score(40, 100); !ok || s != 40 {
		t.Errorf("score 40 of 100: got %v %v", s, ok)
	}
	if s, _ := score(1, 3); s != 33.3 {
		t.Errorf("score rounds to one decimal: got %v", s)
	}
	if _, ok := score(5, 0); ok {
		t.Error("score with nothing identified should be blank")
	}
	if got := matchColumn("FIFA - Category"); got != "fifa" {
		t.Errorf("FIFA header: got %q", got)
	}
	if got := matchColumn("(Yes/No)", "Existing Control (Yes/No)"); got != "existing" {
		t.Errorf("Existing Control yes no: got %q", got)
	}
	if got := matchColumn("Applicable (Yes/No)", "Egress / Ingress Point Applicable (Yes/No)"); got != "point" {
		t.Errorf("egress applicable header: got %q", got)
	}
	if got := matchColumn("Units", "Jan-Apr'26 (Adj. Baseline) Units"); got != "units" {
		t.Errorf("units header: got %q", got)
	}
}

func TestXLSX(t *testing.T) {
	var buf bytes.Buffer
	err := writeXLSX(&buf, []sheet{
		{Name: "Mar 2026", Rows: [][]any{{"Sr No", "Note"}, {int64(7), "<&> =SUM(A1)"}, {3.5, ""}}},
		{Name: "Apr 2026", Rows: [][]any{{"Only"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	book, err := readXLSX(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(book) != 2 || book[0][1][0] != "7" || book[0][1][1] != "<&> =SUM(A1)" || book[0][2][0] != "3.5" || book[1][0][0] != "Only" {
		t.Fatalf("round trip mismatch: %q", book)
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
		_, b := c.get(path)
		c.t.Fatalf("no csrf token on %s: %s", path, b[max(0, len(b)-400):])
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

// multipart posts fields and files (name to content) and returns the final page after redirects.
func (c *client) multipart(path string, fields map[string]string, fileField string, files map[string]string) (int, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	for name, content := range files {
		fw, _ := mw.CreateFormFile(fileField, name)
		fw.Write([]byte(content))
	}
	mw.Close()
	res, err := c.c.Post(c.base+path, mw.FormDataContentType(), &buf)
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

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestPortal(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if db2, err := openDB(dir); err != nil { // migrations are idempotent
		t.Fatal(err)
	} else {
		db2.Close()
	}
	app, err := newApp(Config{DataDir: dir, MaxUploadMB: 5}, db)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 3, 10, 9, 0, 0, 0, time.Local)
	app.now = func() time.Time { return clock }
	type mail struct{ to, subject, body string }
	var sent []mail
	app.mailer = func(s Settings, to, subject, body string) error {
		sent = append(sent, mail{to, subject, body})
		return nil
	}
	sentTo := func(to, subject string) int {
		n := 0
		for _, m := range sent {
			if m.to == to && strings.Contains(m.subject, subject) {
				n++
			}
		}
		return n
	}
	srv := httptest.NewServer(app.routes())
	defer srv.Close()

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
	if code, body := admin.post("/account/password", url.Values{"csrf": {tok}, "current": {temp}, "new": {"golden lager 2026"}, "confirm": {"golden lager 2026"}}); code != 200 || !strings.Contains(body, "Evidence overview") {
		t.Fatalf("password change failed: %d", code)
	}
	tok = admin.csrf("/")

	// People and controls: Sr No 7 owned by Alex, Sr No 9 owned by Sam.
	for _, p := range [][2]string{{"Alex Owner", "alex@brewdat.local"}, {"Sam Other", "sam@brewdat.local"}} {
		admin.post("/admin/users", url.Values{"csrf": {tok}, "name": {p[0]}, "email": {p[1]}, "role": {"user"}})
	}
	var alexID, samID int64
	db.QueryRow(`SELECT id FROM users WHERE email = 'alex@brewdat.local'`).Scan(&alexID)
	db.QueryRow(`SELECT id FROM users WHERE email = 'sam@brewdat.local'`).Scan(&samID)
	db.Exec(`UPDATE users SET must_change = 0, password_hash = (SELECT password_hash FROM users WHERE email = 'admin@brewdat.local') WHERE role = 'user'`)
	admin.post("/admin/controls", url.Values{"csrf": {tok}, "code": {"7"}, "title": {"Access review"}, "description": {"Export of the reviewed access list"},
		"f_fifa": {"Insider risk"}, "f_applicable": {"Yes"}, "accepted": {"10"}, "owners": {itoa(alexID)}})
	admin.post("/admin/controls", url.Values{"csrf": {tok}, "code": {"9"}, "title": {"Cloud uploads"}, "owners": {itoa(samID)}})
	var controlID int64
	if err := db.QueryRow(`SELECT id FROM controls WHERE code = 'B07'`).Scan(&controlID); err != nil {
		t.Fatal("control not created")
	}
	cpath := "/controls/" + itoa(controlID)
	db.Exec(`INSERT INTO settings (key, value) VALUES ('smtp_host', 'mail.test'), ('kickoff_day', '1'), ('reminder_days', '7')`)

	// Owner submits: identified becomes a fixed baseline.
	alex := newClient(t, srv.URL)
	alex.login("alex@brewdat.local", "golden lager 2026")
	upload := func(fields map[string]string, files map[string]string) string {
		fields["csrf"] = alex.csrf(cpath)
		fields["period"] = "202603"
		_, body := alex.multipart(cpath+"/evidence", fields, "files", files)
		return body
	}
	upload(map[string]string{"identified": "100", "remediated": "40", "note": "First month"}, map[string]string{"access_list.pdf": "%PDF fake evidence"})
	upload(map[string]string{"identified": "999", "remediated": "45"}, map[string]string{"update.txt": "more"})
	var ident int64
	db.QueryRow(`SELECT identified FROM controls WHERE id = ?`, controlID).Scan(&ident)
	if ident != 100 {
		t.Fatalf("identified baseline should stay 100, got %d", ident)
	}
	if body := upload(map[string]string{"remediated": "95"}, map[string]string{"x.txt": "x"}); !strings.Contains(body, "cannot be more than identified") {
		t.Error("remediated plus accepted above identified was not refused")
	}
	var fileID int64
	db.QueryRow(`SELECT id FROM files WHERE orig_name = 'access_list.pdf'`).Scan(&fileID)
	if code, body := alex.get("/files/" + itoa(fileID)); code != 200 || body != "%PDF fake evidence" {
		t.Fatalf("download failed: %d %q", code, body)
	}
	if _, body := alex.get("/"); !strings.Contains(body, "Submitted") {
		t.Error("dashboard should show the control as submitted")
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
	if code, _ := alex.get("/admin/review"); code != 403 {
		t.Errorf("user on admin page: got %d, want 403", code)
	}
	if code, _ := admin.post("/admin/users", url.Values{"name": {"X"}, "email": {"x@brewdat.local"}, "role": {"user"}}); code != 403 {
		t.Errorf("post without csrf: got %d, want 403", code)
	}

	// Review queue shows the newest submission with a nav badge; deny needs a reason.
	var subID int64
	db.QueryRow(`SELECT MAX(id) FROM submissions WHERE control_id = ?`, controlID).Scan(&subID)
	_, body = admin.get("/admin/review")
	if !strings.Contains(body, `class="badge"`) || !strings.Contains(body, "Identified 100, remediated 45, accepted risk 10, pending 45") {
		t.Fatalf("review page missing badge or figures")
	}
	review := func(decision, reason string) string {
		_, body := admin.post("/admin/submissions/"+itoa(subID)+"/review", url.Values{"csrf": {tok}, "decision": {decision}, "reason": {reason}, "from": {"queue"}})
		return body
	}
	review("deny", "")
	if st := app.latestStatus(controlID, 202603); st != "pending" {
		t.Fatalf("deny without a reason should be refused, status %s", st)
	}
	review("deny", "The export is missing the sign off page")
	if sentTo("alex@brewdat.local", "Evidence denied") != 1 || !strings.Contains(sent[len(sent)-1].body, "missing the sign off page") {
		t.Fatalf("denial email not sent with reason: %v", sent)
	}

	// Scheduler: Sam gets the kickoff; Alex is not reminded on the day of the denial, then is after 7 days.
	app.tick(clock)
	if sentTo("sam@brewdat.local", "Evidence requested") != 1 || sentTo("alex@brewdat.local", "Reminder") != 0 {
		t.Fatalf("first tick: %v", sent)
	}
	app.tick(clock.AddDate(0, 0, 7))
	alexReminder := ""
	for _, m := range sent {
		if m.to == "alex@brewdat.local" && strings.Contains(m.subject, "Reminder") {
			alexReminder = m.body
		}
	}
	if sentTo("alex@brewdat.local", "Reminder") != 1 || !strings.Contains(alexReminder, "denied, upload again") {
		t.Fatalf("expected a reminder for the denied control: %v", sent)
	}

	// Ticks in the simulated future also clear expired sessions, so sign in again.
	relogin := func() {
		alex.login("alex@brewdat.local", "golden lager 2026")
		admin.login("admin@brewdat.local", "golden lager 2026")
		tok = admin.csrf("/")
	}
	relogin()

	// Alex uploads again; nothing is sent while it waits for review; approval emails and locks the month.
	upload(map[string]string{"remediated": "50"}, map[string]string{"signoff.pdf": "%PDF signed"})
	before := len(sent)
	app.tick(clock.AddDate(0, 0, 15))
	if sentTo("alex@brewdat.local", "Reminder") != 1 {
		t.Fatalf("no reminder expected while waiting for review: %v", sent[before:])
	}
	relogin()
	db.QueryRow(`SELECT MAX(id) FROM submissions WHERE control_id = ?`, controlID).Scan(&subID)
	review("approve", "Sign off page now included")
	if sentTo("alex@brewdat.local", "Evidence approved") != 1 || !strings.Contains(sent[len(sent)-1].body, "Sign off page now included") {
		t.Fatal("approval email not sent with its reason")
	}
	if _, body := admin.get(cpath); strings.Contains(body, "/admin/submissions/"+itoa(subID)+"/review") || !strings.Contains(body, "Approved by Administrator") {
		t.Error("approved submission still shows review buttons or lacks the reviewer")
	}
	if _, body := admin.get(cpath); !strings.Contains(body, "<span>Pending</span><b>40</b>") || !strings.Contains(body, "<b>50%</b>") {
		t.Error("admin control view is missing remediated, pending or score")
	}
	if body := review("deny", "changed my mind"); !strings.Contains(body, "already reviewed") {
		t.Error("an approved submission could be reviewed again")
	}
	if body := upload(map[string]string{"remediated": "60"}, map[string]string{"late.txt": "x"}); !strings.Contains(body, "already approved") {
		t.Error("upload to an approved month was not refused")
	}

	// The Reviewed tab lists both decisions with the reviewer and the reason.
	if _, body := admin.get("/admin/review?show=reviewed"); !strings.Contains(body, "missing the sign off page") || !strings.Contains(body, "Sign off page now included") ||
		!strings.Contains(body, ">Approved<") || !strings.Contains(body, ">Denied<") || !strings.Contains(body, "Administrator") {
		t.Error("reviewed tab is missing decisions")
	}

	// Next month: no change carries the figures forward without files.
	clock = time.Date(2026, 4, 5, 9, 0, 0, 0, time.Local)
	relogin() // sessions from March have expired
	alex.multipart(cpath+"/nochange", map[string]string{"csrf": alex.csrf(cpath), "period": "202604"}, "files", nil)
	var rem int64
	var note string
	db.QueryRow(`SELECT remediated, note FROM latest WHERE control_id = ? AND period = 202604 AND status = 'pending'`, controlID).Scan(&rem, &note)
	if rem != 50 || note != "No change from March 2026." {
		t.Fatalf("no change submission wrong: %d %q", rem, note)
	}

	// Cards show the figures; the executive view shows the gauge and ranked controls.
	if _, body := alex.get("/?period=202604"); !strings.Contains(body, `class="card"`) || !strings.Contains(body, "50%") ||
		!strings.Contains(body, "<b>40</b><span>Pending</span>") || !strings.Contains(body, "BrewDat Exfil Governance") {
		t.Error("dashboard cards or totals missing figures")
	}
	if _, body := admin.get("/admin/controls"); !strings.Contains(body, ">Remediated<") || !strings.Contains(body, "Accepted risk") {
		t.Error("controls list missing figure columns or the accepted risk label")
	}
	if code, body := admin.get("/admin/executive?period=202604"); code != 200 || !strings.Contains(body, "Overall compliance") || !strings.Contains(body, `class="gauge"`) {
		t.Fatalf("executive view: %d", code)
	}
	if code, _ := alex.get("/admin/executive"); code != 403 {
		t.Errorf("user on executive view: got %d, want 403", code)
	}

	// Monthly sheet page and Excel export.
	if code, body := admin.get("/sheet?period=202603"); code != 200 || !strings.Contains(body, "Insider risk") || !strings.Contains(body, ">50<") {
		t.Fatalf("sheet page: %d", code)
	}
	res, err := admin.c.Get(srv.URL + "/sheet.xlsx")
	if err != nil {
		t.Fatal(err)
	}
	xb, _ := io.ReadAll(res.Body)
	res.Body.Close()
	book, err := readXLSX(bytes.NewReader(xb), int64(len(xb)))
	if err != nil || len(book) < 2 || book[0][0][0] != "Control No" {
		t.Fatalf("export unreadable: %v %d", err, len(book))
	}

	// Import: two header rows, one existing control, one new, one unknown owner.
	var wb bytes.Buffer
	writeXLSX(&wb, []sheet{{Name: "Controls", Rows: [][]any{
		{"Sr No", "FIFA - Category", "Control", "Existing Control", "Existing Control", "Owner", "Jan-Apr'26 (Adj. Baseline)", "Jan-Oct'26 (Adj. Baseline) - DLP Risk", "", "", "Data Exfil events (BrewDat)", "Mystery"},
		{"", "", "", "(Yes/No)", "Description", "", "Units", "Identified", "Remediated", "Accepted", "", ""},
		{int64(7), "Insider threat", "Access review", "Y", "DLP in place", "Sam Other", "Controls", int64(300), int64(1), int64(5), int64(0), "x"},
		{int64(8), "Cloud", "Cloud upload control", "no", "", "nobody@brewdat.local", "Events", int64(30), int64(12), int64(0), "", ""},
	}}})
	_, body = admin.multipart("/admin/controls/import", map[string]string{"csrf": tok}, "sheet", map[string]string{"controls.xlsx": wb.String()})
	if !strings.Contains(body, "1 added, 1 updated") || !strings.Contains(body, "nobody@brewdat.local has no account yet") || !strings.Contains(body, "Mystery") {
		t.Fatalf("import report wrong: %s", visibleText(body)[:600])
	}
	assertNoDash(t, "import result", body)
	var fields string
	var acc int64
	db.QueryRow(`SELECT fields, accepted, identified FROM controls WHERE code = 'B07'`).Scan(&fields, &acc, &ident)
	f := decodeFields(fields)
	if f["fifa"] != "Insider threat" || f["existing"] != "Yes" || f["existing_desc"] != "DLP in place" || f["units"] != "Controls" || acc != 5 || ident != 100 {
		t.Fatalf("import update wrong: %v accepted %d identified %d", f, acc, ident)
	}
	var owners, newIdent int64
	db.QueryRow(`SELECT COUNT(*) FROM control_owners WHERE control_id = ?`, controlID).Scan(&owners)
	db.QueryRow(`SELECT identified FROM controls WHERE code = 'B08'`).Scan(&newIdent)
	if owners != 2 || newIdent != 30 {
		t.Fatalf("import owners %d (want 2), new identified %d (want 30)", owners, newIdent)
	}

	// No change works from the imported figures before any month was submitted.
	var c8 int64
	db.QueryRow(`SELECT id FROM controls WHERE code = 'B08'`).Scan(&c8)
	db.Exec(`INSERT INTO control_owners (control_id, user_id) VALUES (?, ?)`, c8, samID)
	sam.login("sam@brewdat.local", "golden lager 2026")
	if _, body := sam.get("/controls/" + itoa(c8)); !strings.Contains(body, "/nochange") {
		t.Error("no change button missing for a control with imported figures")
	}
	db.Exec(`DELETE FROM control_owners WHERE control_id = ? AND user_id = ?`, c8, samID)
	if code, _ := admin.multipart("/controls/"+itoa(c8)+"/nochange", map[string]string{"csrf": tok, "period": "202604"}, "files", nil); code != 403 {
		t.Errorf("admin who is not an owner submitted evidence: got %d, want 403", code)
	}
	if _, body := admin.get("/controls/" + itoa(c8)); strings.Contains(body, "/evidence") {
		t.Error("admin sees the upload form on a control they do not own")
	}
	db.Exec(`INSERT INTO control_owners (control_id, user_id) VALUES (?, ?)`, c8, alexID)
	alex.multipart("/controls/"+itoa(c8)+"/nochange", map[string]string{"csrf": alex.csrf("/controls/" + itoa(c8)), "period": "202604"}, "files", nil)
	db.QueryRow(`SELECT remediated, note FROM latest WHERE control_id = ? AND period = 202604`, c8).Scan(&rem, &note)
	if rem != 12 || note != "No change from the figures on record." {
		t.Fatalf("no change from imported figures wrong: %d %q", rem, note)
	}

	// Templates: edit, reset, and a custom one sent to every owner.
	admin.post("/admin/templates/reminder", url.Values{"csrf": {tok}, "subject": {"Gentle nudge for {month}"}, "body": {"Hi {name}, {count} open: {controls}"}})
	if subj, body := app.emailText("reminder", "Alex", 202604, []string{"  • B07: Access review"}, ""); subj != "Gentle nudge for April 2026" || !strings.Contains(body, "Hi Alex, 1 control open") {
		t.Fatalf("edited reminder not used: %q %q", subj, body)
	}
	admin.post("/admin/templates/reminder/delete", url.Values{"csrf": {tok}})
	if subj, _ := app.emailText("reminder", "Alex", 202604, nil, ""); subj != "Reminder: evidence still needed for April 2026" {
		t.Fatalf("reset reminder wrong: %q", subj)
	}
	admin.post("/admin/templates", url.Values{"csrf": {tok}, "name": {"Audit notice"}, "subject": {"Audit for {month}"}, "body": {"Hello {name}, {controls}"}})
	var customKey string
	db.QueryRow(`SELECT key FROM email_templates WHERE name = 'Audit notice'`).Scan(&customKey)
	admin.post("/admin/templates/"+customKey+"/send", url.Values{"csrf": {tok}, "audience": {"all"}})
	if sentTo("alex@brewdat.local", "Audit for April 2026") != 1 || sentTo("sam@brewdat.local", "Audit for April 2026") != 1 {
		t.Fatalf("custom template not sent to every owner")
	}

	// January: the request asks for a new baseline, which owners set once; last year's history keeps its own.
	clock = time.Date(2027, 1, 5, 9, 0, 0, 0, time.Local)
	relogin()
	app.tick(clock)
	if sentTo("alex@brewdat.local", "New year baseline: please set identified for 2027") != 1 {
		t.Fatalf("January baseline request not sent: %v", sent[len(sent)-1])
	}
	if _, body := alex.get(cpath); !strings.Contains(body, `name="identified"`) || strings.Contains(body, "/nochange") {
		t.Error("January should ask for a new baseline and hide no change")
	}
	alex.multipart(cpath+"/evidence", map[string]string{"csrf": alex.csrf(cpath), "period": "202701", "identified": "80", "remediated": "10"}, "files", map[string]string{"jan.txt": "x"})
	var janIdent, oldIdent, idYear int64
	db.QueryRow(`SELECT identified, identified_year FROM controls WHERE id = ?`, controlID).Scan(&janIdent, &idYear)
	db.QueryRow(`SELECT identified FROM submissions WHERE control_id = ? AND period = 202603 ORDER BY id DESC LIMIT 1`, controlID).Scan(&oldIdent)
	if janIdent != 80 || idYear != 2027 || oldIdent != 100 {
		t.Fatalf("baselines wrong: new %d (%d), March kept %d", janIdent, idYear, oldIdent)
	}
	if _, body := admin.get("/sheet?period=202603"); !strings.Contains(body, ">100<") {
		t.Error("March 2026 should still show its own baseline of 100")
	}
	if _, body := alex.get(cpath); !strings.Contains(body, "Identified baseline 2027") {
		t.Error("control page should show the 2027 baseline")
	}

	// Every page, as each role, is free of dashes in visible text.
	for _, p := range []string{"/", cpath, "/sheet", "/sheet?period=202603", "/account", "/admin/controls", "/admin/controls/" + itoa(controlID),
		"/admin/review", "/admin/review?show=reviewed", "/admin/executive", "/admin/executive?period=202604", "/admin/templates", "/admin/users", "/admin/users/" + itoa(alexID), "/admin/settings", "/nope"} {
		_, body := admin.get(p)
		assertNoDash(t, "admin "+p, body)
	}
	for _, p := range []string{"/", cpath, "/sheet", "/account", "/admin/users"} {
		_, body := alex.get(p)
		assertNoDash(t, "user "+p, body)
	}
	for _, m := range sent {
		assertNoDash(t, "email "+m.subject, m.subject+" "+m.body)
	}
}

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// sendHour is the earliest local hour emails go out.
const sendHour = 8

type Settings struct {
	SMTPHost, SMTPPort, SMTPUser, SMTPPass, SMTPFrom, SMTPTLS, BaseURL string
	KickoffDay, ReminderDays                                           int
}

func (a *App) settings() Settings {
	s := Settings{SMTPPort: "587", SMTPTLS: "starttls", KickoffDay: 1, ReminderDays: 7, BaseURL: env("BREWDAT_BASE_URL", "")}
	rows, err := a.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		log.Printf("settings: %v", err)
		return s
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		rows.Scan(&k, &v)
		switch k {
		case "smtp_host":
			s.SMTPHost = v
		case "smtp_port":
			s.SMTPPort = v
		case "smtp_user":
			s.SMTPUser = v
		case "smtp_pass":
			s.SMTPPass = v
		case "smtp_from":
			s.SMTPFrom = v
		case "smtp_tls":
			s.SMTPTLS = v
		case "base_url":
			if v != "" {
				s.BaseURL = v
			}
		case "kickoff_day":
			s.KickoffDay, _ = strconv.Atoi(v)
		case "reminder_days":
			s.ReminderDays, _ = strconv.Atoi(v)
		}
	}
	return s
}

func headerSafe(s string) string { return strings.NewReplacer("\r", "", "\n", "").Replace(s) }

// sendMail delivers one plain text message. Supports STARTTLS (587), implicit TLS (465) and none (internal relays).
func sendMail(s Settings, to, subject, body string) error {
	from := s.SMTPFrom
	if from == "" {
		from = s.SMTPUser
	}
	addr := net.JoinHostPort(s.SMTPHost, s.SMTPPort)
	tlsCfg := &tls.Config{ServerName: s.SMTPHost, MinVersion: tls.VersionTLS12}
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	var conn net.Conn
	var err error
	if s.SMTPTLS == "tls" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsCfg)
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return err
	}
	conn.SetDeadline(time.Now().Add(60 * time.Second))
	c, err := smtp.NewClient(conn, s.SMTPHost)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if s.SMTPTLS == "starttls" {
		if err := c.StartTLS(tlsCfg); err != nil {
			return err
		}
	}
	if s.SMTPUser != "" {
		if err := c.Auth(smtp.PlainAuth("", s.SMTPUser, s.SMTPPass, s.SMTPHost)); err != nil {
			return err
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(buildMessage(from, to, subject, body, time.Now())); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func buildMessage(from, to, subject, body string, now time.Time) []byte {
	var b bytes.Buffer
	domain := "brewdat.local"
	if i := strings.LastIndex(from, "@"); i >= 0 {
		domain = from[i+1:]
	}
	fmt.Fprintf(&b, "From: %s <%s>\r\n", mime.QEncoding.Encode("utf-8", "BrewDat Exfil Governance"), headerSafe(from))
	fmt.Fprintf(&b, "To: <%s>\r\n", headerSafe(to))
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", headerSafe(subject)))
	fmt.Fprintf(&b, "Date: %s\r\n", now.Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s@%s>\r\n", token(12), headerSafe(domain))
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
	qp := quotedprintable.NewWriter(&b)
	qp.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n")))
	qp.Close()
	return b.Bytes()
}

// ---------- scheduler ----------

func (a *App) scheduler(ctx context.Context) {
	t := time.NewTimer(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		a.tick(a.now())
		t.Reset(10 * time.Minute)
	}
}

func (a *App) tick(now time.Time) {
	a.tickMu.Lock()
	defer a.tickMu.Unlock()
	a.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, now.Unix())
	a.lim.prune(now)
	a.backup(now)
	a.sendDue(now)
}

// calendarDays counts midnights crossed between two instants in b's location.
func calendarDays(a, b time.Time) int {
	a = a.In(b.Location())
	ya, ma, da := a.Date()
	yb, mb, db := b.Date()
	return int(time.Date(yb, mb, db, 0, 0, 0, 0, time.UTC).Sub(time.Date(ya, ma, da, 0, 0, 0, 0, time.UTC)).Hours() / 24)
}

// due decides which email, if any, an owner gets right now: "kickoff", "reminder" or "".
// missing counts controls with nothing submitted this month; denied counts controls whose latest
// submission was denied. Controls waiting for review or approved never trigger an email.
func due(now time.Time, kickoffDay, reminderDays int, kickoffSent bool, lastSent time.Time, missing, denied int) string {
	if missing+denied == 0 || now.Hour() < sendHour {
		return ""
	}
	if !kickoffSent && missing > 0 {
		if now.Day() >= kickoffDay {
			return "kickoff"
		}
		return ""
	}
	if reminderDays > 0 && calendarDays(lastSent, now) >= reminderDays {
		return "reminder"
	}
	return ""
}

type owner struct {
	id              int64
	email, name     string
	open            []string
	missing, denied int
	lastDenied      int64
}

func (a *App) sendDue(now time.Time) {
	s := a.settings()
	if s.SMTPHost == "" {
		return
	}
	period := periodOf(now)
	prev := periodOf(time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).AddDate(0, -1, 0))
	rows, err := a.db.Query(`SELECT u.id, u.email, u.name, c.code, c.title, p.period, COALESCE(l.status, ''), COALESCE(l.reviewed_at, 0)
		FROM control_owners o JOIN users u ON u.id = o.user_id JOIN controls c ON c.id = o.control_id
		JOIN (SELECT ?1 AS period UNION ALL SELECT ?2) p
		LEFT JOIN latest l ON l.control_id = c.id AND l.period = p.period
		WHERE u.active = 1 AND (l.status = 'denied' OR (l.id IS NULL AND p.period = ?1))
		ORDER BY u.id, p.period DESC, length(c.code), c.code`, period, prev)
	if err != nil {
		log.Printf("scheduler: %v", err)
		return
	}
	var owners []*owner
	for rows.Next() {
		var o owner
		var code, title, st string
		var p int
		var reviewed int64
		if err := rows.Scan(&o.id, &o.email, &o.name, &code, &title, &p, &st, &reviewed); err != nil {
			log.Printf("scheduler: %v", err)
			continue
		}
		if len(owners) == 0 || owners[len(owners)-1].id != o.id {
			owners = append(owners, &o)
		}
		last := owners[len(owners)-1]
		item := fmt.Sprintf("  • %s: %s", code, title)
		if st == "denied" {
			last.denied++
			last.lastDenied = max(last.lastDenied, reviewed)
			if p == period {
				item += " (denied, upload again)"
			} else {
				item += " (" + periodLabel(p) + " denied, upload again)"
			}
		} else {
			last.missing++
		}
		last.open = append(last.open, item)
	}
	rows.Close()

	for _, o := range owners {
		var kickoff bool
		var last int64
		a.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM email_log WHERE user_id = ?1 AND period = ?2 AND kind = 'kickoff'),
			COALESCE((SELECT MAX(sent_at) FROM email_log WHERE user_id = ?1 AND period = ?2), 0)`, o.id, period).Scan(&kickoff, &last)
		last = max(last, o.lastDenied) // the denial email counts as the last contact
		var lastSent time.Time
		if last > 0 {
			lastSent = time.Unix(last, 0)
		}
		kind := due(now, s.KickoffDay, s.ReminderDays, kickoff, lastSent, o.missing, o.denied)
		if kind == "" {
			continue
		}
		subject, body := a.emailText(kind, o.name, period, o.open, s.BaseURL)
		if err := a.mailer(s, o.email, subject, body); err != nil {
			log.Printf("email %s to %s failed, will retry: %v", kind, o.email, err)
			continue
		}
		if _, err := a.db.Exec(`INSERT INTO email_log (user_id, period, kind, sent_at) VALUES (?, ?, ?, ?)`, o.id, period, kind, now.Unix()); err != nil {
			log.Printf("email log: %v", err)
		}
		log.Printf("email %s sent to %s for %d open controls", kind, o.email, len(o.open))
	}
}

// reviewText is the email an owner gets when an admin approves or denies their evidence.
func (a *App) reviewText(approved bool, name, code, title string, period int, reason, baseURL string, controlID int64) (string, string) {
	key := "denied"
	if approved {
		key = "approved"
		if reason == "" {
			reason = "None given"
		}
	}
	return a.fill(key, map[string]string{
		"name": name, "month": periodLabel(period), "year": strconv.Itoa(period / 100), "code": code,
		"control": code + ": " + title, "reason": reason, "link": portalLink(baseURL, fmt.Sprintf("/controls/%d", controlID)),
	})
}

// emailText fills the monthly request (the baseline request in January) or the reminder for one owner.
func (a *App) emailText(kind, name string, period int, open []string, baseURL string) (string, string) {
	if kind == "kickoff" && period%100 == 1 {
		kind = "baseline"
	}
	return a.fill(kind, map[string]string{
		"name": name, "month": periodLabel(period), "year": strconv.Itoa(period / 100),
		"controls": strings.Join(open, "\n"), "count": countText(len(open)), "link": portalLink(baseURL, ""),
	})
}

// backup writes one consistent copy of the database per day and keeps the newest 7.
// Evidence files are not copied here; back up the whole data directory off the server too.
func (a *App) backup(now time.Time) {
	day := now.Format("20060102")
	if a.lastBak == day {
		return
	}
	dir := filepath.Join(a.cfg.DataDir, "backup")
	path := filepath.Join(dir, "brewdat_"+day+".db")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if _, err := a.db.Exec(`VACUUM INTO ?`, path); err != nil {
			log.Printf("backup: %v", err)
			return
		}
		log.Printf("backup written: %s", path)
	}
	a.lastBak = day
	old, _ := filepath.Glob(filepath.Join(dir, "brewdat_*.db"))
	sort.Strings(old)
	for len(old) > 7 {
		os.Remove(old[0])
		old = old[1:]
	}
}

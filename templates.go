package main

// Email templates: built in ones admins can edit or reset, and custom ones admins add and send.

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
)

type emailTemplate struct {
	Key, Name, Help, Subject, Body string
	Builtin, Edited                bool
}

const signoff = "\n\nThank you,\nBrewDat Exfil Governance\n"

var builtinTemplates = []emailTemplate{
	{Key: "kickoff", Name: "Monthly request", Help: "Sent on the request day each month to owners with controls still to submit.",
		Subject: "Evidence requested for {month}",
		Body:    "Hello {name},\n\nThe {month} evidence collection is now open. Please upload evidence for the controls you own:\n\n{controls}\n\nUpload here: {link}" + signoff},
	{Key: "baseline", Name: "January baseline request", Help: "Replaces the monthly request in January, when owners set the identified baseline for the new year.",
		Subject: "New year baseline: please set identified for {year}",
		Body: "Hello {name},\n\nA new year of evidence collection starts with {month}. For each control you own, please enter this year's Identified figure with your evidence. " +
			"It becomes the fixed baseline for {year}, and compliance is measured against it all year.\n\n{controls}\n\nUpload here: {link}" + signoff},
	{Key: "reminder", Name: "Reminder", Help: "Sent every few days, as set under Settings, to owners whose controls are still missing or denied.",
		Subject: "Reminder: evidence still needed for {month}",
		Body:    "Hello {name},\n\nA friendly reminder: evidence is still needed for {count} in {month}:\n\n{controls}\n\nUpload here: {link}" + signoff},
	{Key: "approved", Name: "Evidence approved", Help: "Sent to the owners when an administrator approves a submission.",
		Subject: "Evidence approved for {month}: {code}",
		Body:    "Hello {name},\n\nYour {month} evidence for {control} was approved. Nothing more is needed for this month.\n\nComments: {reason}" + signoff},
	{Key: "denied", Name: "Evidence denied", Help: "Sent to the owners when an administrator denies a submission.",
		Subject: "Evidence denied for {month}: {code}",
		Body: "Hello {name},\n\nYour {month} evidence for {control} was denied.\n\nReason: {reason}\n\nPlease upload it again here: {link}\n\n" +
			"You will get reminders until new evidence is uploaded." + signoff},
}

var placeholders = []Detail{
	{"{name}", "the owner's name"},
	{"{month}", "the month, such as November 2026"},
	{"{year}", "the year, such as 2026"},
	{"{controls}", "the owner's controls, one per line"},
	{"{count}", "how many controls, such as 3 controls"},
	{"{link}", "the portal address set under Settings"},
	{"{code}", "the control number, such as B04 (approved and denied only)"},
	{"{control}", "the control number and name (approved and denied only)"},
	{"{reason}", "the reviewer's reason (approved and denied only)"},
}

func builtin(key string) (emailTemplate, bool) {
	for _, t := range builtinTemplates {
		if t.Key == key {
			t.Builtin = true
			return t, true
		}
	}
	return emailTemplate{}, false
}

// templates lists the built in templates (with any saved edits) followed by custom ones.
func (a *App) templates() []emailTemplate {
	saved := map[string]emailTemplate{}
	var custom []emailTemplate
	rows, err := a.db.Query(`SELECT key, name, subject, body FROM email_templates ORDER BY name`)
	if err == nil {
		for rows.Next() {
			var t emailTemplate
			rows.Scan(&t.Key, &t.Name, &t.Subject, &t.Body)
			if _, ok := builtin(t.Key); ok {
				saved[t.Key] = t
			} else {
				custom = append(custom, t)
			}
		}
		rows.Close()
	}
	var out []emailTemplate
	for _, t := range builtinTemplates {
		t.Builtin = true
		if s, ok := saved[t.Key]; ok {
			t.Subject, t.Body, t.Edited = s.Subject, s.Body, true
		}
		out = append(out, t)
	}
	return append(out, custom...)
}

func (a *App) template(key string) (emailTemplate, bool) {
	for _, t := range a.templates() {
		if t.Key == key {
			return t, true
		}
	}
	return emailTemplate{}, false
}

// fill replaces placeholders in a template's subject and body.
func (a *App) fill(key string, vars map[string]string) (string, string) {
	t, _ := a.template(key)
	pairs := make([]string, 0, len(vars)*2)
	for k, v := range vars {
		pairs = append(pairs, "{"+k+"}", v)
	}
	r := strings.NewReplacer(pairs...)
	return r.Replace(t.Subject), r.Replace(t.Body)
}

func portalLink(baseURL, path string) string {
	if baseURL == "" {
		return "the BrewDat Exfil Governance portal"
	}
	return baseURL + path
}

func countText(n int) string {
	if n == 1 {
		return "1 control"
	}
	return fmt.Sprintf("%d controls", n)
}

// ---------- admin pages ----------

func (a *App) templatesPage(w http.ResponseWriter, r *http.Request) {
	a.page(w, r, "admin_templates", View{Title: "Email templates", Nav: "settings", D: map[string]any{
		"Templates": a.templates(), "Placeholders": placeholders, "HasSMTP": a.settings().SMTPHost != "",
	}})
}

func templateInput(r *http.Request) (name, subject, body, problem string) {
	name = strings.TrimSpace(r.PostForm.Get("name"))
	subject = strings.TrimSpace(r.PostForm.Get("subject"))
	body = strings.ReplaceAll(strings.TrimSpace(r.PostForm.Get("body")), "\r\n", "\n")
	switch {
	case subject == "" || len(subject) > 200:
		problem = "Give the template a subject of up to 200 characters."
	case body == "" || len(body) > 10000:
		problem = "Give the template a message of up to 10000 characters."
	}
	return
}

func (a *App) createTemplate(w http.ResponseWriter, r *http.Request) {
	name, subject, body, problem := templateInput(r)
	if problem == "" && (name == "" || len(name) > 80) {
		problem = "Give the template a name of up to 80 characters."
	}
	if problem != "" {
		a.redirect(w, r, "/admin/templates", flashErr(problem))
		return
	}
	if _, err := a.db.Exec(`INSERT INTO email_templates (key, name, subject, body) VALUES (?, ?, ?, ?)`, "c"+token(6), name, subject, body); err != nil {
		a.fail(w, r, err)
		return
	}
	a.redirect(w, r, "/admin/templates", "Template "+name+" added.")
}

func (a *App) saveTemplate(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	t, ok := a.template(key)
	if !ok {
		a.notFound(w, r)
		return
	}
	name, subject, body, problem := templateInput(r)
	if t.Builtin || name == "" {
		name = t.Name
	}
	if problem != "" {
		a.redirect(w, r, "/admin/templates", flashErr(problem))
		return
	}
	if _, err := a.db.Exec(`INSERT INTO email_templates (key, name, subject, body) VALUES (?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET name = excluded.name, subject = excluded.subject, body = excluded.body`, key, name, subject, body); err != nil {
		a.fail(w, r, err)
		return
	}
	a.redirect(w, r, "/admin/templates", "Template "+name+" saved.")
}

// deleteTemplate resets a built in template to its default, or removes a custom one.
func (a *App) deleteTemplate(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	t, ok := a.template(key)
	if !ok {
		a.notFound(w, r)
		return
	}
	a.db.Exec(`DELETE FROM email_templates WHERE key = ?`, key)
	if t.Builtin {
		a.redirect(w, r, "/admin/templates", t.Name+" is back to the default text.")
		return
	}
	a.redirect(w, r, "/admin/templates", "Template "+t.Name+" deleted.")
}

// sendTemplate emails a custom template to every owner, or only to owners with controls still open this month.
func (a *App) sendTemplate(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	t, ok := a.template(key)
	if !ok || t.Builtin {
		a.notFound(w, r)
		return
	}
	s := a.settings()
	if s.SMTPHost == "" {
		a.redirect(w, r, "/admin/templates", flashErr("Add an SMTP server under Settings first."))
		return
	}
	openOnly := r.PostForm.Get("audience") == "open"
	period := periodOf(a.now())
	rows, err := a.db.Query(`SELECT u.id, u.email, u.name, c.code, c.title,
		COALESCE((SELECT l.status FROM latest l WHERE l.control_id = c.id AND l.period = ?), '')
		FROM control_owners o JOIN users u ON u.id = o.user_id JOIN controls c ON c.id = o.control_id
		WHERE u.active = 1 ORDER BY u.id, length(c.code), c.code`, period)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	type person struct {
		email, name string
		list        []string
	}
	var people []*person
	byID := map[int64]*person{}
	for rows.Next() {
		var id int64
		var email, name, code, title, st string
		rows.Scan(&id, &email, &name, &code, &title, &st)
		if openOnly && (st == "pending" || st == "approved") {
			continue
		}
		p := byID[id]
		if p == nil {
			p = &person{email: email, name: name}
			byID[id] = p
			people = append(people, p)
		}
		p.list = append(p.list, fmt.Sprintf("  • %s: %s", code, title))
	}
	rows.Close()
	sent, failed := 0, 0
	for _, p := range people {
		subject, body := a.fill(key, map[string]string{
			"name": p.name, "month": periodLabel(period), "year": strconv.Itoa(period / 100),
			"controls": strings.Join(p.list, "\n"), "count": countText(len(p.list)), "link": portalLink(s.BaseURL, ""),
		})
		if err := a.mailer(s, p.email, subject, body); err != nil {
			log.Printf("template %s to %s: %v", key, p.email, err)
			failed++
			continue
		}
		sent++
	}
	msg := fmt.Sprintf("%s sent to %d %s.", t.Name, sent, map[bool]string{true: "owner", false: "owners"}[sent == 1])
	if failed > 0 {
		a.redirect(w, r, "/admin/templates", flashErr(msg+fmt.Sprintf(" %d could not be sent; the server log has details.", failed)))
		return
	}
	a.redirect(w, r, "/admin/templates", msg)
}

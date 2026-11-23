package main

// Control columns, monthly figures, admin review, the monthly sheet, and Excel import and export.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// column describes one spreadsheet column. Adding or renaming a text column is a one line change here.
// Kind: "fixed" lives in its own DB column (or the owners mapping), "long" is a paragraph, "yn" is Yes or No,
// "" is short text. Aliases are normalised spreadsheet headers that differ from the label.
type column struct {
	Key, Label, Kind string
	Aliases          []string
}

var columns = []column{
	{"code", "Control No", "fixed", []string{"srno", "controlno"}},
	{"fifa", "Category", "", []string{"fifacategory"}},
	{"risk", "Risk", "long", nil},
	{"areas", "Control Areas", "", nil},
	{"point", "Egress / Ingress Point applicable", "yn", []string{"applicableyesno", "egressingresspoint"}},
	{"title", "Control", "fixed", nil},
	{"description", "Control Description", "fixed", nil},
	{"existing", "Existing Control", "yn", []string{"existingcontrolyesno"}},
	{"existing_desc", "Existing Control Description", "long", nil},
	{"solution", "Existing Solution (Tool / Process)", "", []string{"existingsolution", "toolprocess"}},
	{"criticality", "Criticality Definition (Exfil Event / Alert)", "long", []string{"exfileventalert", "criticalitydefinition"}},
	{"owners", "Owner", "fixed", []string{"owners"}},
	{"units", "Units", "", nil},
	{"accepted", "Accepted risk", "fixed", []string{"accepted"}},
	{"exfil_events", "Data Exfil events (BrewDat)", "", nil},
	{"deliverables", "Deliverables (What) / Evidences", "long", nil},
	{"approach", "Review / Monitoring Approach (How)", "long", nil},
	{"remarks", "Remarks", "long", nil},
	{"link", "Link", "", nil},
	{"global_req", "Global Requirements", "long", nil},
	{"global_dp", "Global Data Protection comments", "long", nil},
}

var monthCols = []string{"Identified", "Remediated", "Pending", "Compliance score", "Status"}

var (
	nonAlnum   = regexp.MustCompile(`[^a-z0-9]+`)
	ownerSplit = regexp.MustCompile(`[,;/&\n]+`)
)

func norm(s string) string { return nonAlnum.ReplaceAllString(strings.ToLower(s), "") }

// matchColumn maps spreadsheet header candidates to a column key. Monthly figures other than
// Identified are recognised but ignored on import, because they belong to each month's submission.
func matchColumn(cands ...string) string {
	for _, c := range cands {
		n := norm(c)
		if n == "" {
			continue
		}
		for _, col := range columns {
			if n == norm(col.Label) || n == norm(col.Key) {
				return col.Key
			}
			for _, al := range col.Aliases {
				if n == al {
					return col.Key
				}
			}
		}
		for _, m := range []string{"identified", "remediated", "pending", "compliancescore"} {
			if n == m || strings.HasSuffix(n, m) {
				return m
			}
		}
	}
	return ""
}

// ---------- figures ----------

// checkFigures validates a month's numbers. Pending is identified minus remediated minus accepted.
func checkFigures(identified, remediated, accepted int64) string {
	switch {
	case identified < 0 || remediated < 0 || accepted < 0:
		return "Figures cannot be below zero."
	case remediated+accepted > identified:
		return fmt.Sprintf("Remediated plus accepted risk (%d) cannot be more than identified (%d).", remediated+accepted, identified)
	}
	return ""
}

// score is remediated over identified as a percentage with one decimal; ok is false when there is nothing to measure.
func score(remediated, identified int64) (float64, bool) {
	if identified <= 0 {
		return 0, false
	}
	return math.Round(float64(remediated)*1000/float64(identified)) / 10, true
}

func status(sub string, period, current int) (string, string) {
	switch sub {
	case "pending":
		return "Submitted", "wait"
	case "approved":
		return "Approved", "ok"
	case "denied":
		return "Denied", "late"
	case "":
		if period == current {
			return "Due", "due"
		}
	}
	return "Missing", "late"
}

var bCode = regexp.MustCompile(`^[Bb]?0*(\d{1,6})$`)

// controlCode turns 1, 01, B1 and b01 into B01. Anything else is kept as typed.
func controlCode(s string) string {
	s = strings.TrimSpace(s)
	if f, err := strconv.ParseFloat(s, 64); err == nil && f == math.Trunc(f) && f >= 0 {
		s = strconv.FormatInt(int64(f), 10) // Excel numbers such as 7.0
	}
	if m := bCode.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		return fmt.Sprintf("B%02d", n)
	}
	return s
}

func intCell(s string) (int64, bool) {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", ""))
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f != math.Trunc(f) || f < 0 {
		return 0, false
	}
	return int64(f), true
}

// ---------- month rows ----------

type Row struct {
	ID                               int64
	Code, Title, Description, Owners string
	Fields                           map[string]string
	Identified                       sql.NullInt64
	Accepted, Remediated             int64
	SubID, Last                      int64
	Sub, Status, Class               string
	LastRem                          sql.NullInt64 // last approved month, else the imported sheet figure
}

// Fig is a control's current figures: this month's submission unless it was denied, else the last known figures.
type Fig struct {
	Identified, Remediated, Pending, Score string
	Ratio                                  string // 0 to 1, for meters
	Value                                  float64
	HasScore, ThisMonth                    bool
	Band, BandLabel                        string
	ident, rem, pend                       int64
}

func band(score float64, ok bool) (string, string) {
	switch {
	case !ok:
		return "role", "No figures"
	case score >= 90:
		return "ok", "On track"
	case score >= 70:
		return "due", "Watch"
	}
	return "late", "At risk"
}

func (r Row) Fig() Fig {
	f := Fig{Identified: "None", Remediated: "None", Pending: "None", Score: "None", Ratio: "0"}
	var rem int64
	known := false
	switch {
	case r.Sub != "" && r.Sub != "denied":
		rem, known, f.ThisMonth = r.Remediated, true, true
	case r.LastRem.Valid:
		rem, known = r.LastRem.Int64, true
	}
	if r.Identified.Valid {
		f.Identified, f.ident = strconv.FormatInt(r.Identified.Int64, 10), r.Identified.Int64
	}
	if known {
		f.Remediated, f.rem = strconv.FormatInt(rem, 10), rem
	}
	if known && r.Identified.Valid {
		f.pend = r.Identified.Int64 - rem - r.Accepted
		f.Pending = strconv.FormatInt(f.pend, 10)
		if s, ok := score(rem, r.Identified.Int64); ok {
			f.Value, f.HasScore = s, true
			f.Score = strconv.FormatFloat(s, 'f', -1, 64) + "%"
			f.Ratio = strconv.FormatFloat(s/100, 'f', 3, 64)
		}
	}
	f.Band, f.BandLabel = band(f.Value, f.HasScore)
	return f
}

func (r Row) Pending() (int64, bool) {
	if r.Sub == "" || !r.Identified.Valid {
		return 0, false
	}
	return r.Identified.Int64 - r.Remediated - r.Accepted, true
}

// Cells returns one value per column, then the monthly columns. Numbers stay numbers for Excel.
func (r Row) Cells() []any {
	out := make([]any, 0, len(columns)+len(monthCols))
	for _, c := range columns {
		switch c.Key {
		case "code":
			out = append(out, r.Code)
		case "title":
			out = append(out, r.Title)
		case "description":
			out = append(out, r.Description)
		case "owners":
			out = append(out, r.Owners)
		case "accepted":
			out = append(out, r.Accepted)
		default:
			out = append(out, r.Fields[c.Key])
		}
	}
	var ident, rem, pend, sc any = "", "", "", ""
	if r.Identified.Valid {
		ident = r.Identified.Int64
	}
	if r.Sub != "" {
		rem = r.Remediated
		if p, ok := r.Pending(); ok {
			pend = p
		}
		if r.Identified.Valid {
			if s, ok := score(r.Remediated, r.Identified.Int64); ok {
				sc = s
			}
		}
	}
	return append(out, ident, rem, pend, sc, r.Status)
}

func decodeFields(s string) map[string]string {
	m := map[string]string{}
	json.Unmarshal([]byte(s), &m)
	return m
}

func (a *App) monthRows(u *User, period, current int) ([]Row, error) {
	isAdmin := 0
	if u.IsAdmin() {
		isAdmin = 1
	}
	rows, err := a.db.Query(`SELECT c.id, c.code, c.title, c.description, c.fields,
		COALESCE(l.identified, CASE WHEN c.identified_year = ?1 / 100 THEN c.identified ELSE
			(SELECT s.identified FROM submissions s WHERE s.control_id = c.id AND s.period / 100 = ?1 / 100 AND s.identified IS NOT NULL ORDER BY s.id DESC LIMIT 1) END),
		c.accepted,
		COALESCE(l.id, 0), COALESCE(l.status, ''), COALESCE(l.remediated, 0), COALESCE(l.accepted, 0), COALESCE(l.created_at, 0),
		COALESCE((SELECT group_concat(u.name, ', ') FROM control_owners o JOIN users u ON u.id = o.user_id WHERE o.control_id = c.id), ''),
		COALESCE((SELECT p.remediated FROM latest p WHERE p.control_id = c.id AND p.period < ?1 AND p.period / 100 = ?1 / 100 AND p.status = 'approved' ORDER BY p.period DESC LIMIT 1),
			CASE WHEN c.identified_year = ?1 / 100 THEN c.remediated END)
		FROM controls c LEFT JOIN latest l ON l.control_id = c.id AND l.period = ?1
		WHERE ?2 = 1 OR c.id IN (SELECT control_id FROM control_owners WHERE user_id = ?3)
		ORDER BY length(c.code), c.code`, period, isAdmin, u.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		var fields string
		var ctlAccepted, subAccepted int64
		if err := rows.Scan(&r.ID, &r.Code, &r.Title, &r.Description, &fields, &r.Identified, &ctlAccepted,
			&r.SubID, &r.Sub, &r.Remediated, &subAccepted, &r.Last, &r.Owners, &r.LastRem); err != nil {
			return nil, err
		}
		r.Fields = decodeFields(fields)
		r.Accepted = ctlAccepted
		if r.Sub != "" {
			r.Accepted = subAccepted // the month keeps the value it was submitted with
		}
		r.Status, r.Class = status(r.Sub, period, current)
		out = append(out, r)
	}
	return out, rows.Err()
}

// pickPeriod reads ?period= when it is one of the last 12 months, else the current month.
func pickPeriod(r *http.Request, periods []int) int {
	if q, err := strconv.Atoi(r.URL.Query().Get("period")); err == nil {
		for _, p := range periods {
			if p == q {
				return q
			}
		}
	}
	return periods[0]
}

// ---------- dashboard and monthly sheet ----------

func (a *App) dashboard(w http.ResponseWriter, r *http.Request) {
	u, now := currentUser(r), a.now()
	cur, periods := periodOf(now), recentPeriods(now, 12)
	p := pickPeriod(r, periods)
	list, err := a.monthRows(u, p, cur)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var approved, waiting int
	var rem, ident, pend int64
	for _, row := range list {
		switch row.Sub {
		case "approved":
			approved++
		case "pending":
			waiting++
		}
		if f := row.Fig(); f.HasScore {
			rem, ident, pend = rem+f.rem, ident+f.ident, pend+f.pend
		}
	}
	portfolio, hasScore := score(rem, ident)
	a.page(w, r, "dashboard", View{Title: "Overview", Nav: "home", D: map[string]any{
		"Rows": list, "Done": approved, "Waiting": waiting, "Total": len(list), "Open": len(list) - approved - waiting,
		"Period": p, "Current": cur, "Periods": periods, "Today": now.Format("Monday 2 January"),
		"Score": portfolio, "HasScore": hasScore, "Identified": ident, "Remediated": rem, "Pending": pend,
	}})
}

// Meter is one row of a ranked bar list on the executive view.
type Meter struct {
	ID                              int64
	Code, Label, Sub, Score, Ratio  string
	Band, BandLabel, Status, Class  string
	Identified, Remediated, Pending int64
	HasScore                        bool
	value                           float64
}

func (a *App) executive(w http.ResponseWriter, r *http.Request) {
	u, now := currentUser(r), a.now()
	cur, periods := periodOf(now), recentPeriods(now, 12)
	p := pickPeriod(r, periods)
	list, err := a.monthRows(u, p, cur)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var ident, rem, pend int64
	var approved, waiting, scored int
	type agg struct {
		ident, rem, pend int64
		n                int
	}
	cats, order := map[string]*agg{}, []string{}
	var controls []Meter
	for _, row := range list {
		f := row.Fig()
		switch row.Sub {
		case "approved":
			approved++
		case "pending":
			waiting++
		}
		m := Meter{ID: row.ID, Code: row.Code, Label: row.Title, Sub: row.Owners, Score: f.Score, Ratio: f.Ratio,
			Band: f.Band, BandLabel: f.BandLabel, Status: row.Status, Class: row.Class,
			Identified: f.ident, Remediated: f.rem, Pending: f.pend, HasScore: f.HasScore, value: f.Value}
		controls = append(controls, m)
		if !f.HasScore {
			continue
		}
		scored++
		ident, rem, pend = ident+f.ident, rem+f.rem, pend+f.pend
		name := strings.TrimSpace(row.Fields["fifa"])
		if name == "" {
			name = "No category"
		}
		if cats[name] == nil {
			cats[name] = &agg{}
			order = append(order, name)
		}
		c := cats[name]
		c.ident, c.rem, c.pend, c.n = c.ident+f.ident, c.rem+f.rem, c.pend+f.pend, c.n+1
	}
	// Lowest score first so the controls that need attention lead; controls without figures go last.
	sort.SliceStable(controls, func(i, j int) bool {
		if controls[i].HasScore != controls[j].HasScore {
			return controls[i].HasScore
		}
		return controls[i].value < controls[j].value
	})
	var catMeters []Meter
	for _, name := range order {
		c := cats[name]
		s, ok := score(c.rem, c.ident)
		b, bl := band(s, ok)
		unit := "controls"
		if c.n == 1 {
			unit = "control"
		}
		catMeters = append(catMeters, Meter{Label: name, Sub: fmt.Sprintf("%d %s", c.n, unit),
			Score: strconv.FormatFloat(s, 'f', -1, 64) + "%", Ratio: strconv.FormatFloat(s/100, 'f', 3, 64),
			Band: b, BandLabel: bl, Identified: c.ident, Remediated: c.rem, Pending: c.pend, HasScore: ok, value: s})
	}
	sort.SliceStable(catMeters, func(i, j int) bool { return catMeters[i].value < catMeters[j].value })
	overall, hasOverall := score(rem, ident)
	ob, obl := band(overall, hasOverall)
	a.page(w, r, "admin_executive", View{Title: "Executive view", Nav: "executive", D: map[string]any{
		"Period": p, "Periods": periods, "Current": cur,
		"Overall": strconv.FormatFloat(overall, 'f', -1, 64), "HasOverall": hasOverall, "Ratio": strconv.FormatFloat(overall/100, 'f', 4, 64),
		"Band": ob, "BandLabel": obl, "Identified": ident, "Remediated": rem, "Pending": pend,
		"Total": len(list), "Scored": scored, "Approved": approved, "Waiting": waiting, "Open": len(list) - approved - waiting,
		"Cats": catMeters, "Controls": controls,
	}})
}

func (a *App) sheetPage(w http.ResponseWriter, r *http.Request) {
	u, now := currentUser(r), a.now()
	cur, periods := periodOf(now), recentPeriods(now, 12)
	p := pickPeriod(r, periods)
	list, err := a.monthRows(u, p, cur)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.page(w, r, "sheet", View{Title: "Monthly sheet", Nav: "sheet", D: map[string]any{
		"Rows": list, "Columns": columns, "MonthCols": monthCols, "Period": p, "Periods": periods,
	}})
}

func (a *App) sheetExport(w http.ResponseWriter, r *http.Request) {
	u, now := currentUser(r), a.now()
	cur := periodOf(now)
	have := map[int]bool{cur: true}
	rows, err := a.db.Query(`SELECT DISTINCT period FROM submissions`)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	for rows.Next() {
		var p int
		rows.Scan(&p)
		have[p] = true
	}
	rows.Close()
	header := []any{}
	for _, c := range columns {
		header = append(header, c.Label)
	}
	for _, m := range monthCols {
		header = append(header, m)
	}
	var sheets []sheet
	for _, p := range recentPeriods(now, 12) {
		if !have[p] {
			continue
		}
		list, err := a.monthRows(u, p, cur)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		s := sheet{Name: periodTime(p).Format("Jan 2006"), Rows: [][]any{header}}
		for _, row := range list {
			s.Rows = append(s.Rows, row.Cells())
		}
		sheets = append(sheets, s)
	}
	var buf bytes.Buffer
	if err := writeXLSX(&buf, sheets); err != nil {
		a.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", `attachment; filename="brewdat_controls_`+now.Format("20060102")+`.xlsx"`)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Write(buf.Bytes())
}

// ---------- control page, upload and no change ----------

type Submission struct {
	ID, ControlID        int64
	Code, Title          string
	Period               int
	Note, By             string
	CreatedAt            int64
	Status, Label, Class string // Status is pending, approved or denied
	Reason               string
	Remediated, Accepted int64
	Identified           sql.NullInt64
	ReviewedBy           string
	ReviewedAt           int64
	Latest               bool
	Files                []FileRow
}

// Figures is the one line summary shown in history and the review queue.
func (s *Submission) Figures() string {
	if !s.Identified.Valid {
		return ""
	}
	id := s.Identified.Int64
	out := fmt.Sprintf("Identified %d, remediated %d, accepted risk %d, pending %d", id, s.Remediated, s.Accepted, id-s.Remediated-s.Accepted)
	if sc, ok := score(s.Remediated, id); ok {
		out += fmt.Sprintf(", compliance %g%%", sc)
	}
	return out
}

// isOwner reports whether u is mapped as an owner of the control. Only owners submit evidence;
// admins who are not owners can view and review it but not upload.
func (a *App) isOwner(u *User, controlID int64) bool {
	var ok int
	a.db.QueryRow(`SELECT 1 FROM control_owners WHERE control_id = ? AND user_id = ?`, controlID, u.ID).Scan(&ok)
	return ok == 1
}

// canSubmit gates the upload and no change routes: non owners get not found or, for admins, a clear refusal.
func (a *App) canSubmit(w http.ResponseWriter, r *http.Request, controlID int64) bool {
	u := currentUser(r)
	switch {
	case a.isOwner(u, controlID):
		return true
	case u.IsAdmin() && a.canAccess(u, controlID):
		a.errorPage(w, r, http.StatusForbidden, "Only the control's owners can submit evidence. Administrators can view and review it.")
	default:
		a.notFound(w, r)
	}
	return false
}

// baseline is the identified baseline for the year of period: the control's current one when it belongs to
// that year, else the one recorded on that year's submissions. Each January owners set a new one.
func (a *App) baseline(controlID int64, period int) sql.NullInt64 {
	var b sql.NullInt64
	a.db.QueryRow(`SELECT CASE WHEN c.identified_year = ?2 THEN c.identified ELSE
		(SELECT s.identified FROM submissions s WHERE s.control_id = c.id AND s.period / 100 = ?2 AND s.identified IS NOT NULL ORDER BY s.id DESC LIMIT 1) END
		FROM controls c WHERE c.id = ?1`, controlID, period/100).Scan(&b)
	return b
}

func (a *App) latestStatus(controlID int64, period int) string {
	var st string
	a.db.QueryRow(`SELECT status FROM latest WHERE control_id = ? AND period = ?`, controlID, period).Scan(&st)
	return st
}

type Detail struct{ Label, Value string }

func (a *App) controlPage(w http.ResponseWriter, r *http.Request) {
	u, id := currentUser(r), pathID(r)
	if !a.canAccess(u, id) {
		a.notFound(w, r)
		return
	}
	var code, title, desc, fields string
	var lastRem, idYear sql.NullInt64
	var accepted int64
	if err := a.db.QueryRow(`SELECT code, title, description, fields, accepted, remediated, identified_year FROM controls WHERE id = ?`, id).
		Scan(&code, &title, &desc, &fields, &accepted, &lastRem, &idYear); err != nil {
		a.notFound(w, r)
		return
	}
	year := periodOf(a.now()) / 100
	ident := a.baseline(id, year*100+1)
	if idYear.Int64 != int64(year) {
		lastRem = sql.NullInt64{} // last year's figures do not carry into a new year
	}
	f := decodeFields(fields)
	var details []Detail
	for _, c := range columns {
		if c.Kind != "fixed" && strings.TrimSpace(f[c.Key]) != "" {
			details = append(details, Detail{c.Label, f[c.Key]})
		}
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

	subs, err := a.submissions(`WHERE s.control_id = ? ORDER BY s.id DESC, f.id`, id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	now := a.now()
	cur := periodOf(now)
	seen := map[int]bool{}
	curStatus, canCarry := "", false
	for _, s := range subs {
		s.Latest = !seen[s.Period]
		seen[s.Period] = true
		if s.Latest && s.Period == cur {
			curStatus = s.Status
		}
		if s.Latest && s.Period < cur && s.Period/100 == cur/100 && s.Status != "denied" {
			canCarry = true
		}
		if !s.Latest && s.Status == "pending" {
			s.Label, s.Class = "Replaced", "role"
		}
	}
	// Current figures, the same way the cards and executive view work out.
	figRow := Row{Identified: ident, Accepted: accepted, LastRem: lastRem}
	carried := false
	for _, s := range subs {
		switch {
		case !s.Latest:
		case s.Period == cur && figRow.Sub == "":
			figRow.Sub, figRow.Remediated, figRow.Accepted = s.Status, s.Remediated, s.Accepted
		case s.Period < cur && s.Period/100 == cur/100 && s.Status == "approved" && !carried:
			figRow.LastRem, carried = sql.NullInt64{Int64: s.Remediated, Valid: true}, true
		}
	}
	if figRow.Sub == "denied" {
		figRow.Accepted = accepted
	}
	st, cls := status(curStatus, cur, cur)
	a.page(w, r, "control", View{Title: code, Nav: "home", D: map[string]any{
		"ID": id, "Code": code, "ControlTitle": title, "Description": desc, "Owners": strings.Join(owners, ", "),
		"Details": details, "Identified": ident, "Accepted": accepted, "Fig": figRow.Fig(),
		"Subs": subs, "Periods": recentPeriods(now, 12), "Current": cur, "Status": st, "Class": cls,
		"Approved": curStatus == "approved", "CanCarry": ident.Valid && (canCarry || lastRem.Valid), "Year": year,
		"NewYear":   !ident.Valid && cur%100 <= 3 && a.baseline(id, (year-1)*100+12).Valid,
		"CanSubmit": a.isOwner(u, id),
		"MaxMB":     a.cfg.MaxUploadMB, "MaxFiles": maxFiles,
	}})
}

// submissions loads submissions with their files; where filters on s, c and u.
func (a *App) submissions(where string, args ...any) ([]*Submission, error) {
	rows, err := a.db.Query(`SELECT s.id, s.control_id, c.code, c.title, s.period, s.note, s.created_at, u.name,
		s.status, s.deny_reason, s.remediated, s.accepted, COALESCE(s.identified, c.identified), COALESCE(rv.name, ''), s.reviewed_at,
		COALESCE(f.id, 0), COALESCE(f.orig_name, ''), COALESCE(f.size, 0)
		FROM submissions s JOIN controls c ON c.id = s.control_id JOIN users u ON u.id = s.user_id
		LEFT JOIN users rv ON rv.id = s.reviewed_by
		LEFT JOIN files f ON f.submission_id = s.id `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var subs []*Submission
	for rows.Next() {
		var s Submission
		var f FileRow
		if err := rows.Scan(&s.ID, &s.ControlID, &s.Code, &s.Title, &s.Period, &s.Note, &s.CreatedAt, &s.By,
			&s.Status, &s.Reason, &s.Remediated, &s.Accepted, &s.Identified, &s.ReviewedBy, &s.ReviewedAt, &f.ID, &f.Name, &f.Size); err != nil {
			return nil, err
		}
		if len(subs) == 0 || subs[len(subs)-1].ID != s.ID {
			s.Label, s.Class = status(s.Status, 0, 0)
			subs = append(subs, &s)
		}
		if f.ID != 0 {
			last := subs[len(subs)-1]
			last.Files = append(last.Files, f)
		}
	}
	return subs, rows.Err()
}

func (a *App) validPeriod(p int) bool {
	for _, x := range recentPeriods(a.now(), 12) {
		if x == p {
			return true
		}
	}
	return false
}

func (a *App) uploadEvidence(w http.ResponseWriter, r *http.Request) {
	u, id := currentUser(r), pathID(r)
	if !a.canSubmit(w, r, id) {
		return
	}
	back := fmt.Sprintf("/controls/%d", id)
	fail := func(msg string) { a.redirect(w, r, back, flashErr(msg)) }

	period, _ := strconv.Atoi(r.PostForm.Get("period"))
	if !a.validPeriod(period) {
		fail("Choose a period from the list.")
		return
	}
	if a.latestStatus(id, period) == "approved" {
		fail(periodLabel(period) + " is already approved. Ask an administrator if it needs to change.")
		return
	}
	var accepted int64
	a.db.QueryRow(`SELECT accepted FROM controls WHERE id = ?`, id).Scan(&accepted)
	ident := a.baseline(id, period)
	identified := ident.Int64
	if !ident.Valid {
		v, ok := intCell(r.PostForm.Get("identified"))
		if !ok {
			fail(fmt.Sprintf("Enter how many were identified. It becomes the fixed baseline for %d.", period/100))
			return
		}
		identified = v
	}
	remediated, ok := intCell(r.PostForm.Get("remediated"))
	if !ok {
		fail("Enter the total remediated so far as a whole number.")
		return
	}
	if msg := checkFigures(identified, remediated, accepted); msg != "" {
		fail(msg)
		return
	}
	note := strings.TrimSpace(r.PostForm.Get("note"))
	if len(note) > 4000 {
		fail("Keep the note under 4000 characters.")
		return
	}
	if r.MultipartForm == nil || len(r.MultipartForm.File["files"]) == 0 {
		fail("Choose at least one file to upload. If nothing changed, use No change from last month.")
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
	err := a.insertSubmission(id, period, u.ID, note, identified, !ident.Valid, remediated, accepted, func(tx *sql.Tx, sid int64) error {
		for _, s := range done {
			if _, err := tx.Exec(`INSERT INTO files (submission_id, orig_name, stored_name, size, sha256) VALUES (?, ?, ?, ?, ?)`,
				sid, s.name, s.stored, s.size, s.sum); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		cleanup()
		a.fail(w, r, err)
		return
	}
	a.redirect(w, r, back, fmt.Sprintf("Evidence submitted for %s. An administrator will review it.", periodLabel(period)))
}

// insertSubmission records a pending submission and, the first time, the control's identified baseline.
func (a *App) insertSubmission(controlID int64, period int, userID int64, note string, identified int64, setBaseline bool,
	remediated, accepted int64, extra func(*sql.Tx, int64) error) error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if setBaseline {
		// Only moves forward: a late upload for last year never replaces this year's baseline.
		if _, err := tx.Exec(`UPDATE controls SET identified = ?1, identified_year = ?2, remediated = NULL
			WHERE id = ?3 AND (identified IS NULL OR identified_year IS NULL OR identified_year < ?2)`, identified, period/100, controlID); err != nil {
			return err
		}
	}
	res, err := tx.Exec(`INSERT INTO submissions (control_id, period, user_id, note, created_at, remediated, accepted, identified) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		controlID, period, userID, note, a.now().Unix(), remediated, accepted, identified)
	if err != nil {
		return err
	}
	if extra != nil {
		sid, _ := res.LastInsertId()
		if err := extra(tx, sid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (a *App) noChange(w http.ResponseWriter, r *http.Request) {
	u, id := currentUser(r), pathID(r)
	if !a.canSubmit(w, r, id) {
		return
	}
	back := fmt.Sprintf("/controls/%d", id)
	fail := func(msg string) { a.redirect(w, r, back, flashErr(msg)) }
	period, _ := strconv.Atoi(r.PostForm.Get("period"))
	if !a.validPeriod(period) {
		fail("Choose a period from the list.")
		return
	}
	if a.latestStatus(id, period) == "approved" {
		fail(periodLabel(period) + " is already approved. Ask an administrator if it needs to change.")
		return
	}
	var lastRem sql.NullInt64
	var accepted int64
	a.db.QueryRow(`SELECT accepted, CASE WHEN identified_year = ? THEN remediated END FROM controls WHERE id = ?`, period/100, id).Scan(&accepted, &lastRem)
	ident := a.baseline(id, period)
	if !ident.Valid {
		fail(fmt.Sprintf("Set the identified baseline for %d first: submit evidence with this year's Identified figure.", period/100))
		return
	}
	var prev int
	var remediated int64
	note := ""
	err := a.db.QueryRow(`SELECT period, remediated FROM latest WHERE control_id = ? AND period < ? AND period / 100 = ? AND status <> 'denied' ORDER BY period DESC LIMIT 1`,
		id, period, period/100).Scan(&prev, &remediated)
	switch {
	case err == nil:
		note = "No change from " + periodLabel(prev) + "."
	case lastRem.Valid:
		remediated = lastRem.Int64
		note = "No change from the figures on record."
	default:
		fail("There are no earlier figures to carry forward. Enter identified and remediated and upload evidence instead.")
		return
	}
	if msg := checkFigures(ident.Int64, remediated, accepted); msg != "" {
		fail(msg)
		return
	}
	if extra := strings.TrimSpace(r.PostForm.Get("note")); extra != "" && len(extra) <= 4000 {
		note += " " + extra
	}
	if err := a.insertSubmission(id, period, u.ID, note, ident.Int64, false, remediated, accepted, nil); err != nil {
		a.fail(w, r, err)
		return
	}
	a.redirect(w, r, back, fmt.Sprintf("Submitted as no change for %s. An administrator will review it.", periodLabel(period)))
}

// ---------- review ----------

func (a *App) pendingReviews() int {
	var n int
	a.db.QueryRow(`SELECT COUNT(*) FROM latest WHERE status = 'pending'`).Scan(&n)
	return n
}

func (a *App) reviewPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("show") == "reviewed" {
		// ponytail: newest 300 decisions; add paging if the history needs to go further back.
		subs, err := a.submissions(`WHERE s.id IN (SELECT id FROM submissions WHERE status <> 'pending' ORDER BY reviewed_at DESC, id DESC LIMIT 300)
			ORDER BY s.reviewed_at DESC, s.id DESC, f.id`)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		approved, denied := 0, 0
		for _, s := range subs {
			if s.Status == "approved" {
				approved++
			} else {
				denied++
			}
		}
		a.page(w, r, "admin_reviewed", View{Title: "Reviewed", Nav: "review", D: map[string]any{
			"Subs": subs, "Approved": approved, "Denied": denied, "Waiting": a.pendingReviews(),
		}})
		return
	}
	subs, err := a.submissions(`WHERE s.status = 'pending' AND s.id = (SELECT MAX(id) FROM submissions x WHERE x.control_id = s.control_id AND x.period = s.period)
		ORDER BY s.created_at, s.id, f.id`)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	for _, s := range subs {
		s.Latest = true
	}
	a.page(w, r, "admin_review", View{Title: "Review", Nav: "review", D: map[string]any{"Subs": subs}})
}

func (a *App) reviewSubmission(w http.ResponseWriter, r *http.Request) {
	id, reviewer := pathID(r), currentUser(r)
	var cid, submitter int64
	var period int
	var latest bool
	var code, title string
	err := a.db.QueryRow(`SELECT s.control_id, s.period, s.user_id, c.code, c.title,
		s.id = (SELECT MAX(id) FROM submissions x WHERE x.control_id = s.control_id AND x.period = s.period)
		FROM submissions s JOIN controls c ON c.id = s.control_id WHERE s.id = ?`, id).Scan(&cid, &period, &submitter, &code, &title, &latest)
	if err != nil {
		a.notFound(w, r)
		return
	}
	back := fmt.Sprintf("/controls/%d", cid)
	if r.PostForm.Get("from") == "queue" {
		back = "/admin/review"
	}
	decision := r.PostForm.Get("decision")
	reason := strings.TrimSpace(r.PostForm.Get("reason"))
	var current string
	a.db.QueryRow(`SELECT status FROM submissions WHERE id = ?`, id).Scan(&current)
	switch {
	case !latest:
		a.redirect(w, r, back, flashErr("A newer submission replaced this one. Review the newest instead."))
		return
	case current != "pending":
		a.redirect(w, r, back, flashErr("This submission was already reviewed."))
		return
	case len(reason) > 1000:
		a.redirect(w, r, back, flashErr("Keep the reason under 1000 characters."))
		return
	case decision != "approve" && decision != "deny":
		a.redirect(w, r, back, flashErr("Choose approve or deny."))
		return
	case decision == "deny" && (reason == "" || len(reason) > 1000):
		a.redirect(w, r, back, flashErr("Give a reason (up to 1000 characters) so the owner knows what to fix."))
		return
	}
	st := map[string]string{"approve": "approved", "deny": "denied"}[decision]
	if _, err := a.db.Exec(`UPDATE submissions SET status = ?, deny_reason = ?, reviewed_by = ?, reviewed_at = ? WHERE id = ?`,
		st, reason, reviewer.ID, a.now().Unix(), id); err != nil {
		a.fail(w, r, err)
		return
	}
	msg := fmt.Sprintf("%s %s for %s.", map[string]string{"approve": "Approved", "deny": "Denied"}[decision], code, periodLabel(period))
	s := a.settings()
	if s.SMTPHost != "" {
		rows, err := a.db.Query(`SELECT email, name FROM users WHERE active = 1 AND id <> ?
			AND (id = ? OR id IN (SELECT user_id FROM control_owners WHERE control_id = ?))`, reviewer.ID, submitter, cid)
		if err == nil {
			type to struct{ email, name string }
			var list []to
			for rows.Next() {
				var t to
				rows.Scan(&t.email, &t.name)
				list = append(list, t)
			}
			rows.Close()
			failed := false
			for _, t := range list {
				subject, body := a.reviewText(decision == "approve", t.name, code, title, period, reason, s.BaseURL, cid)
				if err := a.mailer(s, t.email, subject, body); err != nil {
					log.Printf("review email to %s: %v", t.email, err)
					failed = true
				}
			}
			if failed {
				msg += " The email to the owner could not be sent."
			} else if len(list) > 0 {
				msg += " The owner was emailed."
			}
		}
	}
	a.redirect(w, r, back, msg)
}

// ---------- admin: controls ----------

func (a *App) adminControls(w http.ResponseWriter, r *http.Request) { a.showControls(w, r, nil) }

func (a *App) showControls(w http.ResponseWriter, r *http.Request, report []string) {
	rows, err := a.db.Query(`SELECT c.id, c.code, c.title,
		(SELECT COUNT(*) FROM submissions s WHERE s.control_id = c.id),
		COALESCE((SELECT group_concat(u.name, ', ') FROM control_owners o JOIN users u ON u.id = o.user_id WHERE o.control_id = c.id), '')
		FROM controls c ORDER BY length(c.code), c.code`)
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
	cur := periodOf(a.now())
	month, err := a.monthRows(currentUser(r), cur, cur)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	figs := map[int64]Fig{}
	for _, m := range month {
		figs[m.ID] = m.Fig()
	}
	a.page(w, r, "admin_controls", View{Title: "Controls", Nav: "controls", D: map[string]any{
		"Rows": list, "Figs": figs, "Users": users, "Columns": columns, "Fields": map[string]string{}, "Report": report,
		"ID": int64(0), "Code": "", "ControlTitle": "", "Description": "", "Accepted": int64(0), "Identified": sql.NullInt64{},
	}})
}

func (a *App) editControl(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	var code, title, desc, fields string
	var subs int
	var ident sql.NullInt64
	var accepted int64
	err := a.db.QueryRow(`SELECT code, title, description, fields, accepted,
		(SELECT COUNT(*) FROM submissions WHERE control_id = c.id) FROM controls c WHERE id = ?`, id).
		Scan(&code, &title, &desc, &fields, &accepted, &subs)
	if err != nil {
		a.notFound(w, r)
		return
	}
	ident = a.baseline(id, periodOf(a.now()))
	users, err := a.activeUsers(id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.page(w, r, "admin_control", View{Title: "Edit " + code, Nav: "controls", D: map[string]any{
		"ID": id, "Code": code, "ControlTitle": title, "Description": desc, "Users": users, "Subs": subs,
		"Columns": columns, "Fields": decodeFields(fields), "Identified": ident, "Accepted": accepted,
	}})
}

func (a *App) saveControl(w http.ResponseWriter, r *http.Request) {
	id := pathID(r) // 0 when creating
	back := "/admin/controls"
	if id != 0 {
		back = fmt.Sprintf("/admin/controls/%d", id)
	}
	fail := func(msg string) { a.redirect(w, r, back, flashErr(msg)) }
	code := controlCode(r.PostForm.Get("code"))
	title := strings.TrimSpace(r.PostForm.Get("title"))
	desc := strings.TrimSpace(r.PostForm.Get("description"))
	if code == "" || title == "" || len(code) > 40 || len(title) > 500 || len(desc) > 8000 {
		fail("Give the control a number such as B01 (up to 40 characters) and a Control name (up to 500).")
		return
	}
	fields := map[string]string{}
	for _, c := range columns {
		if c.Kind == "fixed" {
			continue
		}
		v := strings.TrimSpace(r.PostForm.Get("f_" + c.Key))
		if len(v) > 8000 {
			fail(c.Label + " is too long. Keep it under 8000 characters.")
			return
		}
		if v != "" {
			fields[c.Key] = v
		}
	}
	fieldsJSON, _ := json.Marshal(fields)
	accepted, ok := int64(0), true
	if v := r.PostForm.Get("accepted"); strings.TrimSpace(v) != "" {
		if accepted, ok = intCell(v); !ok {
			fail("Accepted risk must be a whole number, 0 or more.")
			return
		}
	}
	var ident sql.NullInt64
	if id != 0 {
		if v := strings.TrimSpace(r.PostForm.Get("identified")); v != "" {
			n, ok := intCell(v)
			if !ok {
				fail("Identified must be a whole number, 0 or more, or empty so the owner enters it.")
				return
			}
			ident = sql.NullInt64{Int64: n, Valid: true}
		}
	}
	// Keep pending at zero or more for every month that is not yet approved.
	if id != 0 && ident.Valid {
		var maxRem int64
		a.db.QueryRow(`SELECT COALESCE(MAX(remediated), 0) FROM latest WHERE control_id = ? AND period / 100 = ? AND status <> 'approved'`, id, periodOf(a.now())/100).Scan(&maxRem)
		if maxRem+accepted > ident.Int64 {
			fail(fmt.Sprintf("Identified cannot be lower than remediated plus accepted risk (%d) on open submissions.", maxRem+accepted))
			return
		}
	}
	err := func() error {
		tx, err := a.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if id == 0 {
			res, err := tx.Exec(`INSERT INTO controls (code, title, description, fields, accepted, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
				code, title, desc, string(fieldsJSON), accepted, a.now().Unix())
			if err != nil {
				return err
			}
			id, _ = res.LastInsertId()
		} else {
			year := sql.NullInt64{Int64: int64(periodOf(a.now()) / 100), Valid: ident.Valid}
			res, err := tx.Exec(`UPDATE controls SET code = ?, title = ?, description = ?, fields = ?, accepted = ?, identified = ?, identified_year = ? WHERE id = ?`,
				code, title, desc, string(fieldsJSON), accepted, ident, year, id)
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
		fail(fmt.Sprintf("A control numbered %s already exists.", code))
	case errors.Is(err, sql.ErrNoRows):
		a.notFound(w, r)
	case err != nil:
		a.fail(w, r, err)
	default:
		a.redirect(w, r, "/admin/controls", fmt.Sprintf("Control %s saved.", code))
	}
}

// ---------- admin: import ----------

func rowAt(rows [][]string, i int) []string {
	if i < len(rows) {
		return rows[i]
	}
	return nil
}

func cellAt(row []string, i int) string {
	if i >= 0 && i < len(row) {
		return strings.TrimSpace(row[i])
	}
	return ""
}

func yesNo(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "y", "yes", "true":
		return "Yes"
	case "n", "no", "false":
		return "No"
	}
	return s
}

func (a *App) importControls(w http.ResponseWriter, r *http.Request) {
	report, err := a.importWorkbook(r)
	if err != nil {
		report = []string{err.Error()}
	}
	a.showControls(w, r, report)
}

func (a *App) importWorkbook(r *http.Request) ([]string, error) {
	if r.MultipartForm == nil || len(r.MultipartForm.File["sheet"]) == 0 {
		return nil, errors.New("Choose an Excel .xlsx file to import.")
	}
	h := r.MultipartForm.File["sheet"][0]
	f, err := h.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	book, err := readXLSX(f, h.Size)
	if err != nil {
		return nil, errors.New("That file could not be read. Save it as an Excel .xlsx workbook and try again.")
	}

	// Find the first sheet with a Sr No (or Control No) header in its first 10 rows.
	var rows [][]string
	hi, srCol := -1, -1
	for _, s := range book {
		for ri := 0; ri < len(s) && ri < 10 && hi < 0; ri++ {
			for ci, c := range s[ri] {
				if n := norm(c); n == "srno" || n == "controlno" {
					rows, hi, srCol = s, ri, ci
					break
				}
			}
		}
		if hi >= 0 {
			break
		}
	}
	if hi < 0 {
		return nil, errors.New("No sheet has a Sr No or Control No column header. Check the file and try again.")
	}
	top, sub, start := rows[hi], []string(nil), hi+1
	if n := norm(cellAt(rowAt(rows, start), srCol)); start < len(rows) && (n == "" || n == "srno" || n == "controlno") {
		sub, start = rows[start], start+1 // second header row
	}
	width := max(len(top), len(sub))
	keys := make([]string, width)
	used := map[string]bool{}
	var unknown []string
	group := ""
	for c := 0; c < width; c++ {
		t, s := cellAt(top, c), cellAt(sub, c)
		if s == t {
			s = ""
		}
		if t != "" {
			group = t // merged headers repeat over the columns below them
		}
		if t == "" && s == "" {
			continue
		}
		var key string
		if s == "" {
			key = matchColumn(t)
		} else {
			key = matchColumn(group+" "+s, s, t) // "Existing Control" + "Description" before plain "Description"
		}
		switch {
		case key == "":
			unknown = append(unknown, strings.TrimSpace(t+" "+s))
		case key == "pending" || key == "compliancescore" || used[key]:
		default:
			used[key] = true
			keys[c] = key
		}
	}
	if !used["code"] {
		return nil, errors.New("The Sr No or Control No column could not be matched.")
	}

	byKey := map[string]int64{}
	urows, err := a.db.Query(`SELECT id, email, name FROM users WHERE active = 1`)
	if err != nil {
		return nil, err
	}
	for urows.Next() {
		var id int64
		var email, name string
		urows.Scan(&id, &email, &name)
		byKey[strings.ToLower(email)] = id
		byKey[strings.ToLower(name)] = id
	}
	urows.Close()

	tx, err := a.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var added, updated int
	var notes []string
	for ri := start; ri < len(rows); ri++ {
		row := rows[ri]
		vals := map[string]string{}
		for c, k := range keys {
			if k != "" {
				vals[k] = cellAt(row, c)
			}
		}
		code := controlCode(vals["code"])
		if code == "" {
			continue
		}
		line := fmt.Sprintf("Row %d (%s): ", ri+1, code)
		var id int64
		var title, desc, fieldsJSON string
		var accepted int64
		var ident, lastRem, idYear sql.NullInt64
		err := tx.QueryRow(`SELECT id, title, description, fields, accepted, identified, remediated, identified_year FROM controls WHERE code = ?`, code).
			Scan(&id, &title, &desc, &fieldsJSON, &accepted, &ident, &lastRem, &idYear)
		thisYear := int64(periodOf(a.now()) / 100)
		if idYear.Int64 != thisYear {
			ident = sql.NullInt64{} // last year's baseline: the sheet may set this year's
		}
		isNew := errors.Is(err, sql.ErrNoRows)
		if err != nil && !isNew {
			return nil, err
		}
		fields := decodeFields(fieldsJSON)
		for k, v := range vals {
			switch k {
			case "code", "owners":
			case "title":
				if v != "" {
					title = v
				}
			case "description":
				desc = v
			case "accepted":
				if v == "" {
					continue
				}
				if n, ok := intCell(v); ok {
					accepted = n
				} else {
					notes = append(notes, line+"Accepted risk is not a whole number, kept "+strconv.FormatInt(accepted, 10)+".")
				}
			case "identified":
				if n, ok := intCell(v); ok && !ident.Valid {
					ident = sql.NullInt64{Int64: n, Valid: true}
				}
			case "remediated":
				if n, ok := intCell(v); ok {
					lastRem = sql.NullInt64{Int64: n, Valid: true}
				}
			default:
				if v == "" {
					delete(fields, k)
					continue
				}
				if colKind(k) == "yn" {
					v = yesNo(v)
				}
				fields[k] = v
			}
		}
		if title == "" {
			notes = append(notes, line+"no Control text, skipped.")
			continue
		}
		if len(title) > 500 {
			title = title[:500]
		}
		fj, _ := json.Marshal(fields)
		if isNew {
			res, err := tx.Exec(`INSERT INTO controls (code, title, description, fields, accepted, identified, remediated, identified_year, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				code, title, desc, string(fj), accepted, ident, lastRem, sql.NullInt64{Int64: thisYear, Valid: ident.Valid}, a.now().Unix())
			if err != nil {
				return nil, err
			}
			id, _ = res.LastInsertId()
			added++
		} else {
			yearVal := idYear
			if ident.Valid {
				yearVal = sql.NullInt64{Int64: thisYear, Valid: true}
			}
			if !ident.Valid && idYear.Int64 != thisYear {
				// keep last year's baseline untouched when the sheet has none for this year
				tx.QueryRow(`SELECT identified FROM controls WHERE id = ?`, id).Scan(&ident)
			}
			if _, err := tx.Exec(`UPDATE controls SET title = ?, description = ?, fields = ?, accepted = ?, identified = ?, remediated = ?, identified_year = ? WHERE id = ?`,
				title, desc, string(fj), accepted, ident, lastRem, yearVal, id); err != nil {
				return nil, err
			}
			updated++
		}
		for _, o := range ownerSplit.Split(vals["owners"], -1) {
			o = strings.TrimSpace(o)
			switch strings.ToLower(o) {
			case "", "na", "n/a", "none", "tbd", "nil":
				continue
			}
			uid, ok := byKey[strings.ToLower(o)]
			if !ok {
				notes = append(notes, line+"owner "+o+" has no account yet. Add them under People, then import again.")
				continue
			}
			if _, err := tx.Exec(`INSERT OR IGNORE INTO control_owners (control_id, user_id) VALUES (?, ?)`, id, uid); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	out := []string{fmt.Sprintf("Import finished: %d added, %d updated.", added, updated)}
	if len(unknown) > 0 {
		out = append(out, "Columns not recognised and skipped: "+strings.Join(unknown, ", ")+".")
	}
	return append(out, notes...), nil
}

func colKind(key string) string {
	for _, c := range columns {
		if c.Key == key {
			return c.Kind
		}
	}
	return ""
}

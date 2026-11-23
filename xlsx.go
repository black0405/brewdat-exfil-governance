package main

// Minimal .xlsx writer and reader on the standard library, enough for one row per control.

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
)

type sheet struct {
	Name string
	Rows [][]any // string, int64 or float64; the first row is written bold
}

const (
	nsMain = "http://schemas.openxmlformats.org/spreadsheetml/2006/main"
	nsRel  = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
	xmlHdr = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n"
)

func colName(i int) string {
	s := ""
	for i++; i > 0; i = (i - 1) / 26 {
		s = string(rune('A'+(i-1)%26)) + s
	}
	return s
}

func xmlEsc(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

func writeXLSX(w io.Writer, sheets []sheet) error {
	z := zip.NewWriter(w)
	add := func(name, body string) error {
		f, err := z.Create(name)
		if err == nil {
			_, err = io.WriteString(f, xmlHdr+body)
		}
		return err
	}
	var types, books, rels strings.Builder
	for i, s := range sheets {
		n := i + 1
		fmt.Fprintf(&types, `<Override PartName="/xl/worksheets/sheet%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`, n)
		name := strings.Map(func(r rune) rune {
			if strings.ContainsRune(`[]:*?/\`, r) {
				return ' '
			}
			return r
		}, s.Name)
		if len(name) > 31 {
			name = name[:31]
		}
		fmt.Fprintf(&books, `<sheet name="%s" sheetId="%d" r:id="rId%d"/>`, xmlEsc(name), n, n)
		fmt.Fprintf(&rels, `<Relationship Id="rId%d" Type="%s/worksheet" Target="worksheets/sheet%d.xml"/>`, n, nsRel, n)

		var b strings.Builder
		b.WriteString(`<worksheet xmlns="` + nsMain + `"><sheetData>`)
		for ri, row := range s.Rows {
			fmt.Fprintf(&b, `<row r="%d">`, ri+1)
			style := ""
			if ri == 0 {
				style = ` s="1"`
			}
			for ci, v := range row {
				ref := colName(ci) + strconv.Itoa(ri+1)
				switch v := v.(type) {
				case int64:
					fmt.Fprintf(&b, `<c r="%s"%s><v>%d</v></c>`, ref, style, v)
				case float64:
					fmt.Fprintf(&b, `<c r="%s"%s><v>%s</v></c>`, ref, style, strconv.FormatFloat(v, 'f', -1, 64))
				default:
					// Inline strings: no shared string table, and text starting with = never becomes a formula.
					if s := fmt.Sprint(v); s != "" {
						fmt.Fprintf(&b, `<c r="%s" t="inlineStr"%s><is><t xml:space="preserve">%s</t></is></c>`, ref, style, xmlEsc(s))
					}
				}
			}
			b.WriteString(`</row>`)
		}
		b.WriteString(`</sheetData></worksheet>`)
		if err := add(fmt.Sprintf("xl/worksheets/sheet%d.xml", n), b.String()); err != nil {
			return err
		}
	}
	styles := len(sheets) + 1
	parts := []struct{ name, body string }{
		{"[Content_Types].xml", `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
			`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
			`<Default Extension="xml" ContentType="application/xml"/>` +
			`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
			`<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>` +
			types.String() + `</Types>`},
		{"_rels/.rels", `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId1" Type="` + nsRel + `/officeDocument" Target="xl/workbook.xml"/></Relationships>`},
		{"xl/workbook.xml", `<workbook xmlns="` + nsMain + `" xmlns:r="` + nsRel + `"><sheets>` + books.String() + `</sheets></workbook>`},
		{"xl/_rels/workbook.xml.rels", `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` + rels.String() +
			fmt.Sprintf(`<Relationship Id="rId%d" Type="%s/styles" Target="styles.xml"/>`, styles, nsRel) + `</Relationships>`},
		{"xl/styles.xml", `<styleSheet xmlns="` + nsMain + `">` +
			`<fonts count="2"><font><sz val="11"/><name val="Calibri"/></font><font><b/><sz val="11"/><name val="Calibri"/></font></fonts>` +
			`<fills count="2"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill></fills>` +
			`<borders count="1"><border><left/><right/><top/><bottom/><diagonal/></border></borders>` +
			`<cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>` +
			`<cellXfs count="2"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/><xf numFmtId="0" fontId="1" fillId="0" borderId="0" xfId="0" applyFont="1"/></cellXfs>` +
			`</styleSheet>`},
	}
	for _, p := range parts {
		if err := add(p.name, p.body); err != nil {
			return err
		}
	}
	return z.Close()
}

type xlText struct {
	T string `xml:"t"`
	R []struct {
		T string `xml:"t"`
	} `xml:"r"`
}

func (x xlText) String() string {
	s := x.T
	for _, r := range x.R {
		s += r.T
	}
	return s
}

// cellRef turns "B12" into column 1, row 11.
func cellRef(ref string) (int, int, bool) {
	col, i := 0, 0
	for i < len(ref) && ref[i] >= 'A' && ref[i] <= 'Z' {
		col = col*26 + int(ref[i]-'A') + 1
		i++
	}
	row, err := strconv.Atoi(ref[i:])
	if col == 0 || err != nil || row < 1 {
		return 0, 0, false
	}
	return col - 1, row - 1, true
}

// readXLSX returns every worksheet's cells as text, keyed by part name in workbook order (sheet1, sheet2...).
func readXLSX(r io.ReaderAt, size int64) ([][][]string, error) {
	z, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("not an Excel .xlsx file")
	}
	decode := func(f *zip.File, v any) error {
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		return xml.NewDecoder(io.LimitReader(rc, 32<<20)).Decode(v)
	}
	var shared []string
	sheets := map[int]*zip.File{}
	for _, f := range z.File {
		switch {
		case f.Name == "xl/sharedStrings.xml":
			var sst struct {
				SI []xlText `xml:"si"`
			}
			if err := decode(f, &sst); err != nil {
				return nil, err
			}
			for _, si := range sst.SI {
				shared = append(shared, si.String())
			}
		case strings.HasPrefix(f.Name, "xl/worksheets/sheet") && strings.HasSuffix(f.Name, ".xml"):
			n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(f.Name, "xl/worksheets/sheet"), ".xml"))
			if err == nil {
				sheets[n] = f
			}
		}
	}
	var out [][][]string
	for n := 1; n <= len(sheets)+50 && len(out) < len(sheets); n++ {
		f := sheets[n]
		if f == nil {
			continue
		}
		var ws struct {
			Rows []struct {
				N     int `xml:"r,attr"`
				Cells []struct {
					R  string `xml:"r,attr"`
					T  string `xml:"t,attr"`
					V  string `xml:"v"`
					IS xlText `xml:"is"`
				} `xml:"c"`
			} `xml:"sheetData>row"`
			Merges []struct {
				Ref string `xml:"ref,attr"`
			} `xml:"mergeCells>mergeCell"`
		}
		if err := decode(f, &ws); err != nil {
			return nil, err
		}
		var rows [][]string
		for _, row := range ws.Rows {
			var cells []string
			for _, c := range row.Cells {
				col := len(cells)
				if c.R != "" {
					col = 0
					for _, ch := range c.R {
						if ch < 'A' || ch > 'Z' {
							break
						}
						col = col*26 + int(ch-'A') + 1
					}
					col--
				}
				if col < 0 || col > 1000 {
					continue
				}
				for len(cells) <= col {
					cells = append(cells, "")
				}
				switch c.T {
				case "s":
					if i, err := strconv.Atoi(c.V); err == nil && i >= 0 && i < len(shared) {
						cells[col] = shared[i]
					}
				case "inlineStr":
					cells[col] = c.IS.String()
				default:
					cells[col] = c.V
				}
				cells[col] = strings.TrimSpace(cells[col])
			}
			at := len(rows)
			if row.N > 0 && row.N <= 100000 {
				at = row.N - 1
			}
			for len(rows) <= at {
				rows = append(rows, nil)
			}
			rows[at] = cells
		}
		// Merged cells keep their value only in the top left cell; copy it across the range.
		for _, m := range ws.Merges {
			c1, r1, ok1 := cellRef(strings.Split(m.Ref, ":")[0])
			c2, r2, ok2 := cellRef(m.Ref[strings.LastIndex(m.Ref, ":")+1:])
			if !ok1 || !ok2 || r1 >= len(rows) || c1 >= len(rows[r1]) || r2-r1 > 5000 || c2-c1 > 200 {
				continue
			}
			v := rows[r1][c1]
			for r := r1; r <= r2 && r < len(rows); r++ {
				for c := c1; c <= c2; c++ {
					for len(rows[r]) <= c {
						rows[r] = append(rows[r], "")
					}
					if rows[r][c] == "" {
						rows[r][c] = v
					}
				}
			}
		}
		out = append(out, rows)
	}
	return out, nil
}

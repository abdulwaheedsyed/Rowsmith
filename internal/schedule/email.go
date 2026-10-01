package schedule

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"
	texttemplate "text/template"

	"rowsmith/internal/mail"
)

// emailData fills the report, alert and failure emails.
type emailData struct {
	Kind       string // result, alert, failure or paused
	Name       string
	Headline   string
	Connection string
	RanAt      string
	Rows       string
	Duration   string
	Condition  string
	Observed   string
	Error      string
	Columns    []string
	Preview    [][]string
	MoreRows   string
	MoreCols   int
	FileNote   string
	URL        string
	Why        string
}

func (d emailData) Accent() string {
	switch d.Kind {
	case "alert":
		return "#C2410C"
	case "failure", "paused":
		return "#B42318"
	}
	return "#2F5D50"
}

func (d emailData) Label() string {
	switch d.Kind {
	case "alert":
		return "Alert"
	case "failure":
		return "Run failed"
	case "paused":
		return "Schedule paused"
	}
	return "Scheduled report"
}

var htmlEmail = template.Must(template.New("email").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.Name}}</title></head>
<body style="margin:0;padding:0;background:#EEF0F2;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background:#EEF0F2;padding:24px 12px;">
<tr><td align="center">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:680px;background:#FFFFFF;border-radius:10px;border:1px solid #DADDE1;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;color:#1B1F24;">
<tr><td style="height:4px;background:{{.Accent}};border-radius:10px 10px 0 0;font-size:0;line-height:0;">&nbsp;</td></tr>
<tr><td style="padding:20px 26px 0;">` + mail.Brand + `</td></tr>
<tr><td style="padding:18px 26px 6px;">
  <div style="font-size:12px;font-weight:700;letter-spacing:.06em;text-transform:uppercase;color:{{.Accent}};">{{.Label}}</div>
  <div style="font-size:21px;font-weight:700;margin:6px 0 2px;line-height:1.3;">{{.Name}}</div>
  <div style="font-size:15px;color:#3D434B;line-height:1.5;">{{.Headline}}</div>
</td></tr>
<tr><td style="padding:12px 26px 4px;">
  <table role="presentation" cellpadding="0" cellspacing="0" style="font-size:13px;color:#59616B;line-height:1.7;">
    <tr><td style="padding-right:14px;">Connection</td><td style="color:#1B1F24;">{{.Connection}}</td></tr>
    <tr><td style="padding-right:14px;">Ran</td><td style="color:#1B1F24;">{{.RanAt}}{{if .Duration}} · took {{.Duration}}{{end}}</td></tr>
    {{if .Rows}}<tr><td style="padding-right:14px;">Rows</td><td style="color:#1B1F24;">{{.Rows}}</td></tr>{{end}}
    {{if .Condition}}<tr><td style="padding-right:14px;">Condition</td><td style="color:#1B1F24;">{{.Condition}}{{if .Observed}} — saw <b>{{.Observed}}</b>{{end}}</td></tr>{{end}}
  </table>
</td></tr>
{{if .Error}}<tr><td style="padding:12px 26px 4px;"><div style="background:#FEF3F2;border:1px solid #FECDCA;border-radius:8px;padding:12px 14px;font-size:13px;color:#7A271A;font-family:SFMono-Regular,Consolas,Menlo,monospace;white-space:pre-wrap;word-break:break-word;">{{.Error}}</div></td></tr>{{end}}
{{if .Columns}}<tr><td style="padding:14px 26px 4px;">
  <div style="overflow-x:auto;border:1px solid #E3E6E9;border-radius:8px;">
  <table cellpadding="0" cellspacing="0" style="border-collapse:collapse;width:100%;font-size:12px;font-family:SFMono-Regular,Consolas,Menlo,monospace;">
    <tr>{{range .Columns}}<th align="left" style="padding:7px 10px;background:#F4F5F7;border-bottom:1px solid #E3E6E9;color:#3D434B;font-weight:600;white-space:nowrap;">{{.}}</th>{{end}}</tr>
    {{range $i, $row := .Preview}}<tr>{{range $row}}<td style="padding:6px 10px;border-bottom:1px solid #F0F1F3;color:#1B1F24;white-space:nowrap;">{{.}}</td>{{end}}</tr>{{end}}
  </table></div>
  {{if or .MoreRows .MoreCols}}<div style="font-size:12px;color:#7A828C;padding-top:6px;">{{if .MoreRows}}{{.MoreRows}}{{end}}{{if and .MoreRows .MoreCols}} · {{end}}{{if .MoreCols}}{{.MoreCols}} more columns in the file{{end}}</div>{{end}}
</td></tr>{{end}}
{{if .FileNote}}<tr><td style="padding:12px 26px 0;font-size:13px;color:#3D434B;">{{.FileNote}}</td></tr>{{end}}
{{if .URL}}<tr><td style="padding:18px 26px 4px;"><a href="{{.URL}}" style="display:inline-block;background:#1B1F24;color:#FFFFFF;text-decoration:none;font-size:14px;font-weight:600;padding:10px 16px;border-radius:7px;">Open in Rowsmith</a></td></tr>{{end}}
<tr><td style="padding:18px 26px 22px;font-size:12px;color:#7A828C;line-height:1.5;border-top:1px solid #F0F1F3;">{{.Why}}</td></tr>
</table>
</td></tr></table>
</body></html>`))

var textEmail = texttemplate.Must(texttemplate.New("email").Parse(`{{.Label}}: {{.Name}}
{{.Headline}}

Connection: {{.Connection}}
Ran: {{.RanAt}}{{if .Duration}} (took {{.Duration}}){{end}}
{{if .Rows}}Rows: {{.Rows}}
{{end}}{{if .Condition}}Condition: {{.Condition}}{{if .Observed}} (saw {{.Observed}}){{end}}
{{end}}{{if .Error}}
{{.Error}}
{{end}}{{if .FileNote}}
{{.FileNote}}
{{end}}{{if .URL}}
Open in Rowsmith: {{.URL}}
{{end}}
--
{{.Why}}
`))

func renderEmail(d emailData) (html, text string, err error) {
	var h, t bytes.Buffer
	if err := htmlEmail.Execute(&h, d); err != nil {
		return "", "", err
	}
	if err := textEmail.Execute(&t, d); err != nil {
		return "", "", err
	}
	return h.String(), t.String(), nil
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

func plural(n int64, one, many string) string {
	s := many
	if n == 1 {
		s = one
	}
	return fmt.Sprintf("%s %s", groupDigits(n), s)
}

func groupDigits(n int64) string {
	s := fmt.Sprint(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

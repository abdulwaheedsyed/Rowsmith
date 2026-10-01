package api

import (
	"bytes"
	"context"
	"encoding/json"
	"html/template"
	"strings"
	"sync"
	"time"

	"rowsmith/internal/id"
	"rowsmith/internal/mail"
	"rowsmith/internal/store"
)

// emailGap is the least time between two emails to one person about one
// query; in-app notifications are not limited.
const emailGap = 5 * time.Minute

var (
	lastEmailMu sync.Mutex
	lastEmail   = map[string]time.Time{}
)

func emailDue(userID, shareID string) bool {
	lastEmailMu.Lock()
	defer lastEmailMu.Unlock()
	k := userID + "|" + shareID
	if t, ok := lastEmail[k]; ok && time.Since(t) < emailGap {
		return false
	}
	lastEmail[k] = time.Now()
	if len(lastEmail) > 10_000 {
		for key, t := range lastEmail {
			if time.Since(t) > emailGap {
				delete(lastEmail, key)
			}
		}
	}
	return true
}

// wantsEmail reads the person's notification preference (on by default).
func wantsEmail(u *store.User) bool {
	var p struct {
		NotifyEmail *bool `json:"notifyEmail"`
	}
	_ = json.Unmarshal(u.Prefs, &p)
	return p.NotifyEmail == nil || *p.NotifyEmail
}

// notify records a notification for each person and emails those who
// want it, in the background.
func (s *Server) notify(ctx context.Context, x *store.QueryShare, actor *store.User, kind string, c *store.Comment, userIDs []string) {
	if len(userIDs) == 0 {
		return
	}
	commentID := ""
	if c != nil {
		commentID = c.ID
	}
	for _, uid := range userIDs {
		n := &store.Notification{ID: id.New(), Kind: kind, ActorID: actor.ID, ShareID: x.ID, CommentID: commentID}
		if err := s.store.Notify(ctx, n, uid); err != nil {
			s.log.Error("could not record a notification", "err", err)
		}
	}
	bg := context.WithoutCancel(ctx)
	go func() {
		ctx, cancel := context.WithTimeout(bg, 2*time.Minute)
		defer cancel()
		set := s.scheduleSettings(ctx)
		if !set.Mail.Ready() {
			return
		}
		users := s.people(ctx)
		for _, uid := range userIDs {
			u, ok := users[uid]
			if !ok || u.Disabled || !wantsEmail(u) || !emailDue(uid, x.ID) {
				continue
			}
			subject, html, text := s.notificationEmail(x, actor, kind, c, users)
			if err := mail.Send(ctx, set.Mail, mail.Message{To: []string{u.Email}, Subject: subject, HTML: html, Text: text}); err != nil {
				s.log.Warn("could not email a notification", "to", u.Email, "err", err)
			}
		}
	}()
}

type noteEmail struct {
	Headline string
	Title    string
	Line     int
	Excerpt  string
	Body     string
	URL      string
}

var noteHTML = template.Must(template.New("note").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"></head>
<body style="margin:0;padding:0;background:#EEF0F2;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background:#EEF0F2;padding:24px 12px;"><tr><td align="center">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:600px;background:#FFFFFF;border:1px solid #DADDE1;border-radius:10px;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;color:#1B1F24;">
<tr><td style="padding:22px 26px 0;">` + mail.Brand + `</td></tr>
<tr><td style="padding:18px 26px 4px;">
  <div style="font-size:15px;color:#3D434B;">{{.Headline}}</div>
  <div style="font-size:20px;font-weight:700;margin-top:4px;line-height:1.3;">{{.Title}}</div>
</td></tr>
{{if .Excerpt}}<tr><td style="padding:12px 26px 0;">
  <div style="font-size:12px;color:#7A828C;margin-bottom:4px;">Line {{.Line}}</div>
  <div style="background:#F4F5F7;border-radius:6px;padding:8px 10px;font-family:SFMono-Regular,Consolas,Menlo,monospace;font-size:12px;white-space:pre-wrap;word-break:break-word;">{{.Excerpt}}</div>
</td></tr>{{end}}
{{if .Body}}<tr><td style="padding:14px 26px 0;"><div style="border-left:3px solid #D9A21B;padding:2px 0 2px 12px;font-size:14px;line-height:1.55;white-space:pre-wrap;word-break:break-word;">{{.Body}}</div></td></tr>{{end}}
{{if .URL}}<tr><td style="padding:18px 26px 4px;"><a href="{{.URL}}" style="display:inline-block;background:#1B1F24;color:#FFFFFF;text-decoration:none;font-size:14px;font-weight:600;padding:10px 16px;border-radius:7px;">Open the discussion</a></td></tr>{{end}}
<tr><td style="padding:18px 26px 22px;font-size:12px;color:#7A828C;border-top:1px solid #F0F1F3;">You can turn these emails off in Rowsmith under Account → Notifications.</td></tr>
</table></td></tr></table></body></html>`))

func (s *Server) notificationEmail(x *store.QueryShare, actor *store.User, kind string, c *store.Comment, users map[string]*store.User) (subject, html, text string) {
	d := noteEmail{Title: x.Title}
	switch kind {
	case "shared":
		d.Headline = actor.Name + " shared a query with you"
		subject = actor.Name + " shared “" + x.Title + "” with you"
		d.Body = x.Description
	case "mention":
		d.Headline = actor.Name + " mentioned you"
		subject = actor.Name + " mentioned you on “" + x.Title + "”"
	case "reply":
		d.Headline = actor.Name + " replied"
		subject = actor.Name + " replied on “" + x.Title + "”"
	default:
		d.Headline = actor.Name + " commented on your query"
		subject = actor.Name + " commented on “" + x.Title + "”"
	}
	if base := s.baseURL(); base != "" {
		d.URL = base + "q/" + x.ID
		if c != nil {
			d.URL += "#c-" + c.ID
		}
	}
	if c != nil {
		d.Body = mentionText(c.Body, users)
		if c.Line > 0 {
			lines := strings.Split(x.Body, "\n")
			if c.Line <= len(lines) {
				d.Line, d.Excerpt = c.Line, strings.TrimRight(lines[c.Line-1], " \t\r")
			}
		}
	}
	var b bytes.Buffer
	_ = noteHTML.Execute(&b, d)
	text = d.Headline + ": " + x.Title + "\n\n"
	if d.Excerpt != "" {
		text += "Line " + itoa(d.Line) + ": " + d.Excerpt + "\n\n"
	}
	if d.Body != "" {
		text += d.Body + "\n\n"
	}
	if d.URL != "" {
		text += d.URL + "\n\n"
	}
	text += "--\nYou can turn these emails off in Rowsmith under Account → Notifications.\n"
	return subject, b.String(), text
}

package schedule

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

var errPrivateAddress = errors.New("the webhook points at a private or local network address, which an admin has not allowed")

// Ranges that are not on the public internet, beyond what net.IP reports.
var reserved = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64 can reach IPv4 private space
	netip.MustParsePrefix("2001:db8::/32"),
}

func publicIP(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() {
		return false
	}
	for _, p := range reserved {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// ValidateWebhook checks a webhook URL before it is saved.
func ValidateWebhook(raw string, allowPrivate bool) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || len(raw) > 2048 {
		return errors.New("enter the webhook as a full URL, e.g. https://hooks.slack.com/services/…")
	}
	if u.User != nil {
		return errors.New("put credentials for the webhook in its URL path or query, not before the host")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !allowPrivate {
			return errors.New("webhooks must use https")
		}
	default:
		return errors.New("webhooks must use https")
	}
	if a, err := netip.ParseAddr(strings.Trim(u.Hostname(), "[]")); err == nil && !allowPrivate && !publicIP(a) {
		return errPrivateAddress
	}
	return nil
}

// webhookClient refuses private addresses at connection time, after DNS,
// so a public name that resolves to an internal address is caught too.
func webhookClient(allowPrivate bool) *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second, Control: func(network, address string, _ syscall.RawConn) error {
		if allowPrivate {
			return nil
		}
		ap, err := netip.ParseAddrPort(address)
		if err != nil || !publicIP(ap.Addr()) {
			return errPrivateAddress
		}
		return nil
	}}
	return &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{DialContext: d.DialContext, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second,
			MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}

// webhookKind picks the payload shape a chat service expects.
func webhookKind(u *url.URL) string {
	h := strings.ToLower(u.Hostname())
	switch {
	case h == "hooks.slack.com":
		return "slack"
	case (h == "discord.com" || h == "discordapp.com" || strings.HasSuffix(h, ".discord.com")) && strings.Contains(u.Path, "/api/webhooks/"):
		return "discord"
	case h == "chat.googleapis.com":
		return "gchat"
	case strings.HasSuffix(h, ".webhook.office.com") || h == "outlook.office.com" || strings.HasSuffix(h, ".logic.azure.com") || strings.HasSuffix(h, ".powerplatform.com"):
		return "teams"
	}
	return "json"
}

// Event is what a webhook receives.
type Event struct {
	Event      string     `json:"event"` // result, alert or failure
	Text       string     `json:"text"`  // a one-line summary, for chat tools
	Schedule   EventRef   `json:"schedule"`
	Connection string     `json:"connection"`
	RanAt      string     `json:"ranAt"`
	Rows       int64      `json:"rows"`
	Condition  string     `json:"condition,omitempty"`
	Observed   string     `json:"observed,omitempty"`
	Error      string     `json:"error,omitempty"`
	Columns    []string   `json:"columns,omitempty"`
	Preview    [][]string `json:"preview,omitempty"`
}

type EventRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
}

func postWebhook(ctx context.Context, raw string, allowPrivate bool, ev Event) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if err := ValidateWebhook(raw, allowPrivate); err != nil {
		return err
	}
	text := ev.Text
	if ev.Schedule.URL != "" {
		text += "\n" + ev.Schedule.URL
	}
	var body any
	switch webhookKind(u) {
	case "slack", "gchat", "teams":
		body = map[string]string{"text": text}
	case "discord":
		body = map[string]string{"content": clip(text, 1900)}
	default:
		body = ev
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, raw, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Rowsmith-Scheduler/1")
	resp, err := webhookClient(allowPrivate).Do(req)
	if err != nil {
		if errors.Is(err, errPrivateAddress) || strings.Contains(err.Error(), errPrivateAddress.Error()) {
			return errPrivateAddress
		}
		return fmt.Errorf("could not reach the webhook: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("the webhook answered %d %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

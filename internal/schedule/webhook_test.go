package schedule

import (
	"encoding/json"
	"testing"
)

func TestWebhookBody(t *testing.T) {
	ev := Event{Event: "result", Text: "Districts report: 6 rows", Schedule: EventRef{ID: "s1", URL: "https://db.example.com/schedules/s1"}}
	enc := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	cases := []struct{ kind, avatar, want string }{
		{"slack", "https://db.example.com/icon-512.png", `{"text":"Districts report: 6 rows\nhttps://db.example.com/schedules/s1"}`},
		{"discord", "https://db.example.com/icon-512.png", `{"avatar_url":"https://db.example.com/icon-512.png","content":"Districts report: 6 rows\nhttps://db.example.com/schedules/s1","username":"Rowsmith"}`},
		{"discord", "", `{"content":"Districts report: 6 rows\nhttps://db.example.com/schedules/s1","username":"Rowsmith"}`},
	}
	for _, c := range cases {
		if got := enc(webhookBody(c.kind, ev, c.avatar)); got != c.want {
			t.Errorf("%s (avatar %q):\n got %s\nwant %s", c.kind, c.avatar, got, c.want)
		}
	}
	if got := webhookBody("generic", ev, "x"); got.(Event).Text != ev.Text {
		t.Errorf("generic webhooks get the whole event, got %v", got)
	}
}

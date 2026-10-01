package web

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLinkPreviewImage(t *testing.T) {
	page := func(publicBase string) string {
		rec := httptest.NewRecorder()
		Handler("/sql/", publicBase).ServeHTTP(rec, httptest.NewRequest("GET", "/sql/connections", nil))
		b, _ := io.ReadAll(rec.Body)
		return string(b)
	}
	body := page("https://db.example.com/sql/")
	if strings.Contains(body, "was not built") {
		t.Skip("the web UI is not built into this test binary")
	}
	if !strings.Contains(body, `<base href="/sql/">`) || !strings.Contains(body, `<meta property="og:image" content="https://db.example.com/sql/social-preview.png" />`) {
		t.Fatalf("missing base or preview image:\n%s", body[:min(len(body), 1500)])
	}
	if strings.Contains(page(""), "og:image") {
		t.Fatal("without a public URL there is no absolute image URL to give")
	}
}

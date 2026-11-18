package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestSubscriptionBindingDoesNotEchoInput(t *testing.T) {
	const private = "subscription-secret-field"
	for _, body := range []string{`{"mode": {"` + private + `": true}}`, `{"url": https://example.com/` + private + `}`} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPut, "/api/outbound-proxies/subscription", strings.NewReader(body))
		if _, ok := bindSubscription(c); ok || w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), private) {
			t.Fatalf("malformed subscription response: status=%d body=%s", w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/outbound-proxies/subscription", strings.NewReader(`{"mode":"manual","format":"webshare","protocol":"socks5h","interval_minutes":60,"clear_url":true}`))
	if req, ok := bindSubscription(c); !ok || req.Mode != "manual" || req.IntervalMinutes != 60 || !req.ClearURL {
		t.Fatal("failed to bind subscription configuration")
	}
}

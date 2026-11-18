package outboundproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gpt-load/internal/encryption"
	"gpt-load/internal/httpclient"
	"gpt-load/internal/models"
)

func TestSubscriptionParsingAndFullResponse(t *testing.T) {
	rows, err := parseSubscription([]byte("\ufeff# comment\r\n\n[2001:DB8::1]:1080:a@b:secret:/?#\n2001:db8::1:1080:a@b:secret:/?#"), "webshare", "socks5h")
	if err != nil || len(rows) != 1 || rows[0] != "socks5h://a%40b:secret%3A%2F%3F%23@[2001:db8::1]" {
		t.Fatalf("unexpected Webshare parse: %v %v", rows, err)
	}
	longHost := strings.Repeat(strings.Repeat("a", 63)+".", 3) + strings.Repeat("b", 61)
	cases := []string{
		"# empty\n", "http://invalid:0\n", "http://a:secret@example.com\nsecret-password",
		"http://u:a@EXAMPLE.com:80\nhttp://u:b@example.com.", strings.Repeat("http://example.com\n", 2001),
		strings.Repeat("#", maxSubscriptionBytes+1), "http://" + longHost + ":8080",
	}
	for i, body := range cases {
		if rows, err := parseSubscription([]byte(body), "url_list", "socks5h"); rows != nil || err == nil || strings.Contains(err.Error(), "secret-password") {
			t.Errorf("invalid case %d: rows=%d err=%v", i, len(rows), err)
		}
	}
	input := SubscriptionInput{subscriptionSettings: subscriptionSettings{Format: "url_list"}}
	res := &http.Response{StatusCode: http.StatusPartialContent, Body: io.NopCloser(strings.NewReader("http://example.com"))}
	if _, err := readSubscriptionResponse(res, input); err == nil {
		t.Fatal("accepted partial list")
	}
	res.StatusCode = http.StatusOK
	if _, err := readSubscriptionResponse(res, input); err != nil {
		t.Fatal(err)
	}
}

func TestSubscriptionDownloadAddresses(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "::ffff:127.0.0.1", "169.254.169.254", "100.64.0.1", "192.0.0.1", "168.63.129.16", "198.18.0.1", "240.0.0.1", "fd00::1", "2002::1", "64:ff9b::7f00:1", "3fff::1"} {
		if publicIP(netip.MustParseAddr(raw)) {
			t.Errorf("allowed special address %s", raw)
		}
	}
	for _, raw := range []string{"1.1.1.1", "2606:4700::1"} {
		if !publicIP(netip.MustParseAddr(raw)) {
			t.Errorf("rejected public address %s", raw)
		}
	}
	if _, err := safeTarget("http://[::ffff:127.0.0.1]/secret-password"); err == nil || strings.Contains(err.Error(), "secret-password") {
		t.Fatalf("unsafe URL rejection: %v", err)
	}
}

func subscriptionFixture(t *testing.T, encrypted bool) (*Manager, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/test.db"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&models.OutboundProxy{}, &models.OutboundProxySubscription{}); err != nil {
		t.Fatal(err)
	}
	key := ""
	if encrypted {
		key = "test-key-strong-long-1234567890"
	}
	svc, err := encryption.NewService(key)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(db, svc, httpclient.NewHTTPClientManager())
	if err := m.LoadSubscription(encrypted); err != nil {
		t.Fatal(err)
	}
	return m, db
}

func subscriptionInput() SubscriptionInput {
	return SubscriptionInput{subscriptionSettings: subscriptionSettings{"subscription", "url_list", "socks5h", false, 60}, URL: "https://example.com/list?token=secret-password"}
}

func TestSubscriptionTransactionalTakeoverAndStableIDs(t *testing.T) {
	m, db := subscriptionFixture(t, true)
	old, err := m.Create("Keep name", "http://u:old@example.com", false)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := m.Create("Remove", "socks5h://remove.example", true)
	if err != nil {
		t.Fatal(err)
	}
	candidates := []string{"http://u:new@example.com", "socks5h://added.example"}
	m.fetchSubscription = func(context.Context, SubscriptionInput) ([]string, error) { return candidates, nil }
	in := subscriptionInput()
	if _, err := m.SaveSubscription(in); err == nil {
		t.Fatal("takeover requires preview")
	}
	p, err := m.PreviewSubscription(context.Background(), in)
	if err != nil || p.Diff != (Diff{Added: 1, Updated: 1, Removed: 1}) {
		t.Fatalf("preview = %+v, %v", p.Diff, err)
	}
	in.PreviewToken = p.Token
	if err := db.Exec(`CREATE TRIGGER fail_config BEFORE INSERT ON outbound_proxy_subscriptions BEGIN SELECT RAISE(ABORT, 'fail config'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := m.SaveSubscription(in); err == nil {
		t.Fatal("takeover succeeded despite trigger")
	}
	var persisted []models.OutboundProxy
	if err := db.Order("id").Find(&persisted).Error; err != nil || len(persisted) != 2 || persisted[1].ID != deleted.ID || persisted[0].URLHash != old.URLHash {
		t.Fatalf("pool was not rolled back, err=%v", err)
	}
	proxies, _ := m.List()
	if proxies[0].URL != old.URL || m.Subscription().Mode != "manual" {
		t.Fatal("published failed transaction")
	}
	if err := db.Exec(`DROP TRIGGER fail_config`).Error; err != nil {
		t.Fatal(err)
	}
	status, err := m.SaveSubscription(in)
	if err != nil || !status.HasURL || status.LastResult == nil || *status.LastResult != p.Diff {
		t.Fatalf("apply failed: %+v %v", status, err)
	}
	var record models.OutboundProxySubscription
	if err := db.First(&record).Error; err != nil || record.URL == in.URL || record.URL == "" {
		t.Fatal("URL not stored encrypted")
	}
	data, _ := json.Marshal(status)
	if strings.Contains(string(data), "secret-password") || strings.Contains(string(data), "preview_token") || !strings.Contains(string(data), `"mode":"subscription"`) {
		t.Fatalf("unexpected status JSON: %s", data)
	}
	proxies, _ = m.List()
	if len(proxies) != 2 || proxies[0].ID != old.ID || proxies[0].Name != old.Name || proxies[0].Enabled || proxies[0].URL != candidates[0] || !proxies[1].Enabled {
		t.Fatal("proxy metadata or membership changed incorrectly")
	}
	before, err := m.Select("sticky key")
	if err != nil {
		t.Fatal(err)
	}
	candidates = []string{candidates[1], candidates[0]}
	diff, err := m.RefreshSubscription(context.Background())
	after, _ := m.Select("sticky key")
	if err != nil || diff != (Diff{Unchanged: 2}) || before.ID != after.ID {
		t.Fatalf("no-op refresh: %+v %v", diff, err)
	}
	if _, err := m.Create("one", "http://example.org", true); err == nil {
		t.Fatal("allowed create in subscription mode")
	}
	if _, err := m.CreateBatch([]BatchItem{{URL: "http://example.org"}}); err == nil {
		t.Fatal("allowed batch in subscription mode")
	}
	if _, err := m.Update(old.ID, nil, &in.URL, nil); err == nil {
		t.Fatal("allowed address edit in subscription mode")
	}
	if err := m.Delete(old.ID); err == nil {
		t.Fatal("allowed delete in subscription mode")
	}
	name, enabled := "renamed", true
	if p, err := m.Update(old.ID, &name, nil, &enabled); err != nil || p.Name != name || !p.Enabled {
		t.Fatalf("allowed edit failed: %v", err)
	}
	in.Mode, in.URL = "manual", ""
	status, err = m.SaveSubscription(in)
	afterSwitch, _ := m.List()
	if err != nil || !status.HasURL || len(afterSwitch) != 2 {
		t.Fatalf("switch to manual: %+v %v", status, err)
	}
	in.ClearURL, in.URL = true, "https://example.com/replacement"
	if _, err := m.SaveSubscription(in); err == nil {
		t.Fatal("accepted replacement URL while clearing")
	}
	in.URL, in.Mode = "", "subscription"
	if _, err := m.SaveSubscription(in); err == nil {
		t.Fatal("allowed clearing in subscription mode")
	}
	in.Mode = "manual"
	status, err = m.SaveSubscription(in)
	if err != nil || status.HasURL || status.LastResult != nil || status.LastSuccessAt != nil {
		t.Fatalf("clear: %+v %v", status, err)
	}
	afterClear, _ := m.List()
	if len(afterClear) != len(afterSwitch) {
		t.Fatal("clear changed proxy membership")
	}
}

func TestSubscriptionPreviewInvalidationAndAmbiguousIdentity(t *testing.T) {
	m, _ := subscriptionFixture(t, true)
	ctx := context.Background()
	first, err := m.Create("first", "http://u:old@example.com", true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Create("second", "http://u:other@example.com", true)
	if err != nil {
		t.Fatal(err)
	}
	candidate := "http://u:third@example.com"
	m.fetchSubscription = func(context.Context, SubscriptionInput) ([]string, error) { return []string{candidate}, nil }
	in := subscriptionInput()
	if _, err := m.PreviewSubscription(ctx, in); err == nil {
		t.Fatal("ambiguous password selected automatically")
	}
	candidate = second.URL
	p, err := m.PreviewSubscription(ctx, in)
	if err != nil || p.Diff != (Diff{Removed: 1, Unchanged: 1}) {
		t.Fatalf("exact match: %+v %v", p.Diff, err)
	}
	if err := m.Delete(first.ID); err != nil {
		t.Fatal(err)
	}
	in.PreviewToken = p.Token
	if _, err := m.SaveSubscription(in); err == nil {
		t.Fatal("used stale preview")
	}
}

func TestSubscriptionConfigurationAndShutdownCancelOldRefresh(t *testing.T) {
	m, _ := subscriptionFixture(t, true)
	ctx := context.Background()
	first := "http://one.example"
	m.fetchSubscription = func(context.Context, SubscriptionInput) ([]string, error) { return []string{first}, nil }
	in := subscriptionInput()
	p, err := m.PreviewSubscription(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	in.PreviewToken = p.Token
	if _, err := m.SaveSubscription(in); err != nil {
		t.Fatal(err)
	}
	for _, stop := range []bool{false, true} {
		started := make(chan struct{})
		m.fetchSubscription = func(ctx context.Context, _ SubscriptionInput) ([]string, error) {
			close(started)
			<-ctx.Done()
			return []string{"http://stale.example"}, nil
		}
		result := make(chan error, 1)
		go func() { _, err := m.RefreshSubscription(ctx); result <- err }()
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("refresh did not start")
		}
		if _, err := m.RefreshSubscription(ctx); err == nil {
			t.Fatal("accepted concurrent refresh")
		}
		if stop {
			shutdown, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			if err := m.Stop(shutdown); err != nil {
				t.Fatal(err)
			}
		} else {
			in.IntervalMinutes++
			if _, err := m.SaveSubscription(in); err != nil {
				t.Fatal(err)
			}
		}
		select {
		case err = <-result:
			if err == nil {
				t.Fatal("stale refresh succeeded")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("refresh was not canceled")
		}
		rows, _ := m.List()
		if len(rows) != 1 || rows[0].URL != first {
			t.Fatal("stale refresh changed the pool")
		}
	}
}

func TestSubscriptionRequiresKey(t *testing.T) {
	m, _ := subscriptionFixture(t, false)
	in := subscriptionInput()
	in.Mode = "manual"
	if _, err := m.SaveSubscription(in); err == nil {
		t.Fatal("accepted URL without encryption")
	}
}

func TestSubscriptionStartupUsesSavedPoolWhileRefreshing(t *testing.T) {
	m, db := subscriptionFixture(t, true)
	ctx := context.Background()
	old := "http://stored.example"
	m.fetchSubscription = func(context.Context, SubscriptionInput) ([]string, error) { return []string{old}, nil }
	in := subscriptionInput()
	in.AutoRefresh = true
	p, err := m.PreviewSubscription(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	in.PreviewToken = p.Token
	if _, err := m.SaveSubscription(in); err != nil {
		t.Fatal(err)
	}
	restarted := NewManager(db, m.enc, httpclient.NewHTTPClientManager())
	if err := restarted.Reload(); err != nil {
		t.Fatal(err)
	}
	if err := restarted.LoadSubscription(true); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	restarted.fetchSubscription = func(work context.Context, _ SubscriptionInput) ([]string, error) {
		close(started)
		<-work.Done()
		return nil, work.Err()
	}
	restarted.Start()
	defer func() {
		stop, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		if err := restarted.Stop(stop); err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("automatic startup refresh did not start")
	}
	proxies, _ := restarted.List()
	if len(proxies) != 1 || proxies[0].URL != old || !restarted.Subscription().Running {
		t.Fatal("old pool unavailable during asynchronous refresh")
	}
}

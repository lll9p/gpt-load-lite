package outboundproxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	apierrors "gpt-load/internal/errors"
	"gpt-load/internal/models"
)

const subscriptionID = 1

type subscriptionSettings struct {
	Mode            string `json:"mode"`
	Format          string `json:"format"`
	Protocol        string `json:"protocol"`
	AutoRefresh     bool   `json:"auto_refresh"`
	IntervalMinutes int    `json:"interval_minutes"`
}

type SubscriptionInput struct {
	subscriptionSettings
	URL          string `json:"url,omitempty"`
	PreviewToken string `json:"preview_token,omitempty"`
	ClearURL     bool   `json:"clear_url,omitempty"`
}

type Diff struct {
	Added     int `json:"added"`
	Updated   int `json:"updated"`
	Removed   int `json:"removed"`
	Unchanged int `json:"unchanged"`
}

type SubscriptionStatus struct {
	subscriptionSettings
	HasURL        bool       `json:"has_url"`
	Running       bool       `json:"running"`
	LastAttemptAt *time.Time `json:"last_attempt_at,omitempty"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	NextRefreshAt *time.Time `json:"next_refresh_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	LastResult    *Diff      `json:"last_result,omitempty"`
}

type SubscriptionPreview struct {
	Token string `json:"preview_token"`
	Diff
}

type preview struct {
	token   string
	config  SubscriptionInput
	rows    []string
	expires time.Time
}

// All state below is guarded by Manager.mu, which also serializes pool writes.
type subscriptionState struct {
	record     models.OutboundProxySubscription
	source     string
	preview    *preview
	revision   uint64
	next       time.Time
	running    bool
	workCancel context.CancelFunc
	workDone   chan struct{}
	stopped    bool
	stop       context.CancelFunc
	done       chan struct{}
	wake       chan struct{}
}

func subscriptionError(message string) error {
	return apierrors.NewAPIError(apierrors.ErrValidation, message)
}

func (m *Manager) subscriptionDB() *gorm.DB {
	return m.db.Session(&gorm.Session{Logger: logger.Discard})
}

// LoadSubscription runs before the HTTP server and scheduler start.
func (m *Manager) LoadSubscription(hasEncryptionKey bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hasEncryptionKey = hasEncryptionKey
	record := models.OutboundProxySubscription{ID: subscriptionID, Mode: "manual", Format: "webshare", Protocol: "socks5h", IntervalMinutes: 60}
	if err := m.subscriptionDB().First(&record, subscriptionID).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return apierrors.ErrDatabase
	}
	if record.URL != "" {
		if !m.hasEncryptionKey {
			return errors.New("stored subscription URL requires ENCRYPTION_KEY")
		}
		source, err := m.enc.Decrypt(record.URL)
		if err != nil {
			return errors.New("failed to decrypt subscription URL; check ENCRYPTION_KEY")
		}
		if _, err = safeTarget(source); err != nil {
			return errors.New("stored subscription URL is invalid")
		}
		m.sub.source = source
	}
	m.sub.record = record
	return nil
}

func (m *Manager) Subscription() SubscriptionStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.subscriptionStatusLocked()
}

func (m *Manager) subscriptionStatusLocked() SubscriptionStatus {
	r := m.sub.record
	result := SubscriptionStatus{
		subscriptionSettings: subscriptionSettings{r.Mode, r.Format, r.Protocol, r.AutoRefresh, r.IntervalMinutes},
		HasURL:               r.URL != "", Running: m.sub.running, LastAttemptAt: r.LastAttemptAt, LastSuccessAt: r.LastSuccessAt, LastError: r.LastError,
	}
	if !m.sub.next.IsZero() {
		next := m.sub.next
		result.NextRefreshAt = &next
	}
	if r.Result != "" {
		var diff Diff
		if json.Unmarshal([]byte(r.Result), &diff) == nil {
			result.LastResult = &diff
		}
	}
	return result
}

func (m *Manager) resolveSubscriptionLocked(in SubscriptionInput) (SubscriptionInput, error) {
	if (in.Mode != "manual" && in.Mode != "subscription") || (in.Format != "webshare" && in.Format != "url_list") || (in.Protocol != "http" && in.Protocol != "https" && in.Protocol != "socks5" && in.Protocol != "socks5h") || in.IntervalMinutes < 5 || in.IntervalMinutes > 1440 {
		return in, apierrors.ErrValidation
	}
	in.URL = strings.TrimSpace(in.URL)
	if in.ClearURL {
		if in.Mode != "manual" || in.URL != "" {
			return in, subscriptionError("clear_url requires manual mode and no replacement URL")
		}
	} else if in.URL == "" {
		in.URL = m.sub.source
	}
	if in.URL != "" {
		if !m.hasEncryptionKey {
			return in, subscriptionError("configure ENCRYPTION_KEY before saving a subscription URL")
		}
		if _, err := safeTarget(in.URL); err != nil {
			return in, err
		}
	} else if in.Mode == "subscription" {
		return in, subscriptionError("a subscription download URL is required")
	}
	return in, nil
}

func (m *Manager) invalidatePreviewLocked() {
	m.sub.preview = nil
	m.sub.revision++
}

func (m *Manager) PreviewSubscription(ctx context.Context, in SubscriptionInput) (SubscriptionPreview, error) {
	m.mu.Lock()
	in, err := m.resolveSubscriptionLocked(in)
	if err != nil {
		m.mu.Unlock()
		return SubscriptionPreview{}, err
	}
	if in.URL == "" {
		m.mu.Unlock()
		return SubscriptionPreview{}, subscriptionError("a subscription download URL is required")
	}
	workCtx, err := m.beginSubscriptionWorkLocked(ctx)
	if err != nil {
		m.mu.Unlock()
		return SubscriptionPreview{}, err
	}
	revision := m.sub.revision
	m.mu.Unlock()

	rows, err := m.fetchSubscription(workCtx, in)
	m.mu.Lock()
	defer m.mu.Unlock()
	defer m.endSubscriptionWorkLocked()
	if workCtx.Err() != nil || revision != m.sub.revision {
		return SubscriptionPreview{}, subscriptionError("subscription preview is stale; preview again")
	}
	if err != nil {
		return SubscriptionPreview{}, err
	}
	plan, err := m.planSync(rows)
	if err != nil {
		return SubscriptionPreview{}, err
	}
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return SubscriptionPreview{}, apierrors.ErrInternalServer
	}
	token := hex.EncodeToString(nonce[:])
	in.PreviewToken = ""
	m.sub.preview = &preview{token: token, config: in, rows: rows, expires: time.Now().Add(5 * time.Minute)}
	return SubscriptionPreview{token, plan.diff}, nil
}

func sameSource(a, b SubscriptionInput) bool {
	return a.URL == b.URL && a.Format == b.Format && a.Protocol == b.Protocol
}

func (m *Manager) SaveSubscription(in SubscriptionInput) (SubscriptionStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sub.stopped {
		return SubscriptionStatus{}, subscriptionError("subscription service is stopping")
	}
	in, err := m.resolveSubscriptionLocked(in)
	if err != nil {
		return SubscriptionStatus{}, err
	}
	current := m.subscriptionStatusLocked().subscriptionSettings
	oldSource := SubscriptionInput{subscriptionSettings: current, URL: m.sub.source}
	changedSource := !sameSource(in, oldSource)
	takeOver := in.Mode == "subscription" && (current.Mode != "subscription" || changedSource)
	if current == in.subscriptionSettings && !changedSource {
		return m.subscriptionStatusLocked(), nil
	}
	var plan *syncPlan
	if takeOver {
		p := m.sub.preview
		if p == nil || p.token != in.PreviewToken || !time.Now().Before(p.expires) || !sameSource(p.config, in) {
			return SubscriptionStatus{}, subscriptionError("preview the current subscription source before applying it")
		}
		plan, err = m.planSync(p.rows)
		if err != nil {
			return SubscriptionStatus{}, err
		}
	}
	r := m.sub.record
	if in.ClearURL {
		r.URL = ""
	} else if in.URL != m.sub.source {
		r.URL, err = m.enc.Encrypt(in.URL)
		if err != nil {
			return SubscriptionStatus{}, apierrors.ErrInternalServer
		}
	}
	r.Mode, r.Format, r.Protocol, r.AutoRefresh, r.IntervalMinutes = in.Mode, in.Format, in.Protocol, in.AutoRefresh, in.IntervalMinutes
	if changedSource {
		r.LastAttemptAt, r.LastSuccessAt, r.LastError, r.Result = nil, nil, "", ""
	}
	if plan != nil {
		now := time.Now().UTC()
		recordSuccess(&r, &now, plan.diff)
	}
	if err := m.commitSubscription(context.Background(), r, plan); err != nil {
		return SubscriptionStatus{}, err
	}
	m.sub.source = in.URL
	m.invalidatePreviewLocked()
	// The commit and cancellation are serialized with the completion of older downloads.
	if m.sub.workCancel != nil {
		m.sub.workCancel()
	}
	m.scheduleLocked(time.Now().Add(time.Duration(in.IntervalMinutes) * time.Minute))
	return m.subscriptionStatusLocked(), nil
}

func recordSuccess(record *models.OutboundProxySubscription, attempt *time.Time, diff Diff) {
	now := time.Now().UTC()
	record.LastAttemptAt, record.LastSuccessAt, record.LastError = attempt, &now, ""
	body, _ := json.Marshal(diff)
	record.Result = string(body)
}

func (m *Manager) commitSubscription(ctx context.Context, record models.OutboundProxySubscription, plan *syncPlan) error {
	var rows []models.OutboundProxy
	err := m.subscriptionDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if plan != nil && plan.changed() {
			if err := m.applySync(tx, plan); err != nil {
				return err
			}
			var err error
			rows, err = m.load(tx)
			if err != nil {
				return err
			}
		}
		return tx.Save(&record).Error
	})
	if err != nil {
		return apierrors.ErrDatabase
	}
	m.sub.record = record
	if plan != nil && plan.changed() {
		m.publish(rows)
		m.invalidatePreviewLocked()
	}
	return nil
}

func (m *Manager) beginSubscriptionWorkLocked(ctx context.Context) (context.Context, error) {
	if m.sub.stopped {
		return nil, subscriptionError("subscription service is stopping")
	}
	if m.sub.workDone != nil {
		return nil, subscriptionError("a subscription request is already in progress")
	}
	work, cancel := context.WithCancel(ctx)
	m.sub.workCancel, m.sub.workDone = cancel, make(chan struct{})
	return work, nil
}

func (m *Manager) endSubscriptionWorkLocked() {
	m.sub.workCancel()
	close(m.sub.workDone)
	m.sub.workDone, m.sub.workCancel = nil, nil
	m.wakeSubscriptionLocked()
}

func (m *Manager) RefreshSubscription(ctx context.Context) (Diff, error) {
	return m.refreshSubscription(ctx, false)
}

func (m *Manager) refreshSubscription(ctx context.Context, scheduled bool) (Diff, error) {
	m.mu.Lock()
	if scheduled && (m.sub.next.IsZero() || time.Now().Before(m.sub.next) || m.sub.workDone != nil) {
		m.mu.Unlock()
		return Diff{}, nil
	}
	if m.sub.record.Mode != "subscription" {
		m.mu.Unlock()
		return Diff{}, subscriptionError("online subscription is not active")
	}
	workCtx, err := m.beginSubscriptionWorkLocked(ctx)
	if err != nil {
		m.mu.Unlock()
		return Diff{}, err
	}
	in := SubscriptionInput{subscriptionSettings: m.subscriptionStatusLocked().subscriptionSettings, URL: m.sub.source}
	attempt := time.Now().UTC()
	m.sub.running, m.sub.next = true, time.Time{}
	m.mu.Unlock()

	candidates, err := m.fetchSubscription(workCtx, in)
	m.mu.Lock()
	defer m.mu.Unlock()
	defer func() {
		m.sub.running = false
		if m.sub.next.IsZero() {
			m.scheduleLocked(time.Now().Add(time.Duration(m.sub.record.IntervalMinutes) * time.Minute))
		}
		m.endSubscriptionWorkLocked()
	}()
	if workCtx.Err() != nil {
		return Diff{}, subscriptionError("subscription refresh was canceled")
	}
	r := m.sub.record
	if err == nil {
		var plan *syncPlan
		plan, err = m.planSync(candidates)
		if err == nil {
			recordSuccess(&r, &attempt, plan.diff)
			err = m.commitSubscription(workCtx, r, plan)
			if err == nil {
				return plan.diff, nil
			}
		}
	}
	r = m.sub.record
	r.LastAttemptAt, r.LastError = &attempt, err.Error()
	if workCtx.Err() == nil && m.commitSubscription(workCtx, r, nil) != nil {
		// Keep useful status in memory even when the database cannot accept writes.
		m.sub.record = r
	}
	return Diff{}, err
}

type proxyChange struct {
	id  uint
	raw string
}
type syncPlan struct {
	diff      Diff
	deleteIDs []uint
	writes    []proxyChange
}

func (p *syncPlan) changed() bool { return len(p.deleteIDs) != 0 || len(p.writes) != 0 }

func (m *Manager) planSync(candidates []string) (*syncPlan, error) {
	byIdentity := make(map[proxyIdentity][]models.OutboundProxy, len(m.snapshot))
	for _, p := range m.snapshot {
		key, _ := identify(p.URL)
		byIdentity[key] = append(byIdentity[key], p)
	}
	plan := &syncPlan{}
	kept := make(map[uint]bool, len(candidates))
	for _, raw := range candidates {
		key, canonical := identify(raw)
		matches := byIdentity[key]
		if len(matches) == 0 {
			plan.diff.Added++
			plan.writes = append(plan.writes, proxyChange{raw: raw})
			continue
		}
		selected := -1
		for i := range matches {
			if matches[i].URL == raw {
				selected = i
				break
			}
		}
		if selected == -1 {
			if len(matches) != 1 {
				return nil, subscriptionError("existing proxies have conflicting credentials; clean up the duplicate identity first")
			}
			selected = 0
		}
		p := matches[selected]
		kept[p.ID] = true
		_, old := identify(p.URL)
		if old == canonical {
			plan.diff.Unchanged++
			continue
		}
		plan.diff.Updated++
		plan.writes = append(plan.writes, proxyChange{p.ID, raw})
	}
	for _, p := range m.snapshot {
		if !kept[p.ID] {
			plan.deleteIDs = append(plan.deleteIDs, p.ID)
		}
	}
	plan.diff.Removed = len(plan.deleteIDs)
	return plan, nil
}

func (m *Manager) applySync(tx *gorm.DB, plan *syncPlan) error {
	for start := 0; start < len(plan.deleteIDs); start += 100 {
		if err := tx.Delete(&models.OutboundProxy{}, plan.deleteIDs[start:min(start+100, len(plan.deleteIDs))]).Error; err != nil {
			return err
		}
	}
	var inserts []models.OutboundProxy
	for _, change := range plan.writes {
		ciphertext, err := m.enc.Encrypt(change.raw)
		if err != nil {
			return err
		}
		hash := m.enc.Hash(change.raw)
		if change.id == 0 {
			u, _ := url.Parse(change.raw)
			inserts = append(inserts, models.OutboundProxy{Name: u.Host, URL: ciphertext, URLHash: hash, Enabled: true})
		} else if err := tx.Model(&models.OutboundProxy{}).Where("id = ?", change.id).Updates(map[string]any{"url": ciphertext, "url_hash": hash}).Error; err != nil {
			return err
		}
	}
	if len(inserts) > 0 {
		return tx.CreateInBatches(&inserts, 100).Error
	}
	return nil
}

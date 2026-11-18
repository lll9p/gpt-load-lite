package models

import "time"

// OutboundProxySubscription is the singleton configuration and last refresh outcome.
// URL holds only ciphertext, even while the pool is in manual mode.
type OutboundProxySubscription struct {
	ID              uint `gorm:"primaryKey;autoIncrement:false"`
	Mode            string
	Format          string
	Protocol        string
	URL             string `gorm:"type:text"`
	AutoRefresh     bool
	IntervalMinutes int
	LastAttemptAt   *time.Time
	LastSuccessAt   *time.Time
	LastError       string
	Result          string
}

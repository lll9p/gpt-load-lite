package commands

import (
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gpt-load/internal/encryption"
	"gpt-load/internal/models"
)

func TestMigrateKeysReencryptsSubscriptionAndProtectsDowngrade(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/migration.db"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&models.OutboundProxySubscription{}); err != nil {
		t.Fatal(err)
	}
	oldKey, newKey := "old-test-key-long-1234567890", "new-test-key-long-1234567890"
	old, err := encryption.NewService(oldKey)
	if err != nil {
		t.Fatal(err)
	}
	url := "https://example.com/path?secret-token=private"
	ciphertext, err := old.Encrypt(url)
	if err != nil {
		t.Fatal(err)
	}
	row := models.OutboundProxySubscription{ID: 1, Mode: "manual", URL: ciphertext}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	if err := NewMigrateKeysCommand(db, nil, oldKey, "").Execute(); err == nil || !strings.Contains(err.Error(), "cannot disable encryption") || strings.Contains(err.Error(), url) {
		t.Fatalf("downgrade error: %v", err)
	}
	if err := db.First(&row).Error; err != nil || row.URL != ciphertext {
		t.Fatal("blocked downgrade changed stored URL")
	}
	if err := NewMigrateKeysCommand(db, nil, oldKey, newKey).Execute(); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&row).Error; err != nil || row.URL == ciphertext || row.URL == url {
		t.Fatal("URL was not reencrypted")
	}
	current, err := encryption.NewService(newKey)
	if err != nil {
		t.Fatal(err)
	}
	if plain, err := current.Decrypt(row.URL); err != nil || plain != url {
		t.Fatal("rotated URL not readable")
	}
	if err := db.Model(&row).Update("url", "").Error; err != nil {
		t.Fatal(err)
	}
	if err := NewMigrateKeysCommand(db, nil, newKey, "").Execute(); err != nil {
		t.Fatal(err)
	}
}

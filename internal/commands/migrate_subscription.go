package commands

import (
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gpt-load/internal/encryption"
	"gpt-load/internal/models"
)

func (cmd *MigrateKeysCommand) subscriptionURL(db *gorm.DB, source encryption.Service) (string, error) {
	if !cmd.hasSubscriptionTable {
		return "", nil
	}
	db = db.Session(&gorm.Session{Logger: logger.Discard})
	var values []string
	if err := db.Model(&models.OutboundProxySubscription{}).Where("id = ?", 1).Pluck("url", &values).Error; err != nil {
		return "", fmt.Errorf("failed to read subscription URL")
	}
	if len(values) == 0 || values[0] == "" {
		return "", nil
	}
	if cmd.toKey == "" {
		return "", fmt.Errorf("cannot disable encryption while a subscription URL is stored (including in manual mode)")
	}
	if cmd.fromKey == "" {
		return "", fmt.Errorf("stored subscription URL is encrypted; specify --from")
	}
	value, err := source.Decrypt(values[0])
	if err != nil || value == "" {
		return "", fmt.Errorf("failed to decrypt subscription URL; check --from")
	}
	return value, nil
}

func (cmd *MigrateKeysCommand) switchSubscriptionURL(tx *gorm.DB) error {
	if !cmd.hasSubscriptionTable {
		return nil
	}
	oldService, newService, err := cmd.createMigrationServices()
	if err != nil {
		return err
	}
	value, err := cmd.subscriptionURL(tx, oldService)
	if err != nil || value == "" {
		return err
	}
	encrypted, err := newService.Encrypt(value)
	if err != nil {
		return fmt.Errorf("failed to encrypt subscription URL")
	}
	verified, err := newService.Decrypt(encrypted)
	if err != nil || verified != value {
		return fmt.Errorf("failed to verify migrated subscription URL")
	}
	result := tx.Session(&gorm.Session{Logger: logger.Discard}).Model(&models.OutboundProxySubscription{}).Where("id = ?", 1).Update("url", encrypted)
	if result.Error != nil || result.RowsAffected != 1 {
		return fmt.Errorf("failed to switch subscription URL")
	}
	return nil
}

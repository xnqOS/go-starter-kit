package migrations

import (
	"time"

	"gorm.io/gorm"
)

// This snapshot is immutable after application. Do not import the live model.
type exampleV000001 struct {
	ID        string    `gorm:"primaryKey;type:text;index:m_example_created_at_idx,sort:desc,priority:2"`
	Name      string    `gorm:"not null;type:text;check:m_example_name_check,length(name) BETWEEN 1 AND 200"`
	CreatedAt time.Time `gorm:"not null;autoCreateTime:false;index:m_example_created_at_idx,sort:desc,priority:1"`
	UpdatedAt time.Time `gorm:"not null;autoUpdateTime:false"`
}

func (exampleV000001) TableName() string { return "m_example" }

func createExampleV000001(tx *gorm.DB) error {
	return tx.Migrator().CreateTable(&exampleV000001{})
}

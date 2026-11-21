package example

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/xnqOS/go-starter-kit/kit/apperror"
	"github.com/xnqOS/go-starter-kit/kit/database"
)

var _ Repo = (*SQLRepo)(nil)

// SQLRepo is shared by SQLite and PostgreSQL. ORM details stay in this layer.
type SQLRepo struct{ db *gorm.DB }

func NewSQLRepo(db *gorm.DB) *SQLRepo { return &SQLRepo{db: db} }

type exampleRow struct {
	ID        string `gorm:"primaryKey;type:text"`
	Name      string
	CreatedAt time.Time `gorm:"autoCreateTime:false"`
	UpdatedAt time.Time `gorm:"autoUpdateTime:false"`
}

func (exampleRow) TableName() string { return "m_example" }

func (r *SQLRepo) Create(ctx context.Context, item Example) error {
	row := exampleRow(item)
	return r.db.WithContext(ctx).Create(&row).Error
}

func (r *SQLRepo) Get(ctx context.Context, id string) (Example, error) {
	var row exampleRow
	err := r.db.WithContext(ctx).Where("id = ?", id).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Example{}, apperror.ErrNotFound
	}
	return Example(row), err
}

func (r *SQLRepo) List(ctx context.Context, params ListParams) ([]Example, int64, error) {
	var rows []exampleRow
	var total int64
	// One read snapshot keeps rows and total consistent with each other.
	err := database.ReadTx(ctx, r.db, func(tx *gorm.DB) error {
		// Add filters here only: the page and the total both start from base.
		base := tx.Model(&exampleRow{}).Session(&gorm.Session{})
		err := base.Order("created_at DESC").Order("id DESC").
			Limit(params.Limit).Offset(params.Offset).Find(&rows).Error
		if err != nil {
			return err
		}
		// A nonempty short page reaches the end. An empty page only reveals
		// the total when offset is zero; beyond the end we still need COUNT.
		if len(rows) < params.Limit && (len(rows) > 0 || params.Offset == 0) {
			total = int64(params.Offset) + int64(len(rows))
			return nil
		}
		return base.Count(&total).Error
	})
	if err != nil {
		return nil, 0, err
	}
	items := make([]Example, 0, len(rows))
	for _, row := range rows {
		items = append(items, Example(row))
	}
	return items, total, nil
}

func (r *SQLRepo) Update(ctx context.Context, id, name string, updatedAt time.Time) (Example, error) {
	var row exampleRow
	// Both supported databases implement RETURNING. Map updates also preserve
	// explicit zero values when business fields are added later.
	result := r.db.WithContext(ctx).Model(&row).Clauses(clause.Returning{}).
		Where("id = ?", id).Updates(map[string]any{"name": name, "updated_at": updatedAt})
	if result.Error != nil {
		return Example{}, result.Error
	}
	if result.RowsAffected == 0 {
		return Example{}, apperror.ErrNotFound
	}
	return Example(row), nil
}

func (r *SQLRepo) Delete(ctx context.Context, id string) error {
	result := r.db.WithContext(ctx).Where("id = ?", id).Delete(&exampleRow{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return apperror.ErrNotFound
	}
	return nil
}

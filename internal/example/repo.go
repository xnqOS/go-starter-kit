package example

import (
	"context"
	"time"
)

// Repo is the persistence capability required by this module's service.
type Repo interface {
	Create(context.Context, Example) error
	Get(context.Context, string) (Example, error)
	List(context.Context, ListParams) (items []Example, total int64, err error)
	Update(ctx context.Context, id, name string, updatedAt time.Time) (Example, error)
	Delete(context.Context, string) error
}

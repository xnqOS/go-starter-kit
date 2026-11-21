package example

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/xnqOS/go-starter-kit/kit/apperror"
)

var _ Repo = (*MemoryRepo)(nil)

type MemoryRepo struct {
	mu    sync.RWMutex
	items map[string]Example
}

func NewMemoryRepo() *MemoryRepo {
	return &MemoryRepo{items: make(map[string]Example)}
}

func (r *MemoryRepo) Create(ctx context.Context, item Example) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items[item.ID] = item
	return nil
}

func (r *MemoryRepo) Get(ctx context.Context, id string) (Example, error) {
	if err := ctx.Err(); err != nil {
		return Example{}, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	item, ok := r.items[id]
	if !ok {
		return Example{}, apperror.ErrNotFound
	}
	return item, nil
}

func (r *MemoryRepo) List(ctx context.Context, params ListParams) ([]Example, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	r.mu.RLock()
	items := make([]Example, 0, len(r.items))
	for _, item := range r.items {
		items = append(items, item)
	}
	r.mu.RUnlock()
	total := int64(len(items))
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].ID > items[j].ID
		}
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})
	if params.Offset >= len(items) {
		return []Example{}, total, nil
	}
	end := params.Offset + min(params.Limit, len(items)-params.Offset)
	return items[params.Offset:end], total, nil
}

func (r *MemoryRepo) Update(ctx context.Context, id, name string, updatedAt time.Time) (Example, error) {
	if err := ctx.Err(); err != nil {
		return Example{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.items[id]
	if !ok {
		return Example{}, apperror.ErrNotFound
	}
	item.Name, item.UpdatedAt = name, updatedAt
	r.items[id] = item
	return item, nil
}

func (r *MemoryRepo) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.items[id]; !ok {
		return apperror.ErrNotFound
	}
	delete(r.items, id)
	return nil
}

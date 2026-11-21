package example

import (
	"context"
	"crypto/rand"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xnqOS/go-starter-kit/kit/apperror"
)

type Service struct {
	repo Repo
	now  func() time.Time // replaced in tests for deterministic timestamps
}

func NewService(repo Repo) *Service { return &Service{repo: repo, now: time.Now} }

func validName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 200 {
		return "", apperror.Invalid("name must contain 1 to 200 characters")
	}
	return name, nil
}

func (s *Service) Create(ctx context.Context, name string) (Example, error) {
	name, err := validName(name)
	if err != nil {
		return Example{}, err
	}
	now := s.now().UTC()
	item := Example{ID: rand.Text(), Name: name, CreatedAt: now, UpdatedAt: now}
	if err := s.repo.Create(ctx, item); err != nil {
		return Example{}, err
	}
	return item, nil
}

func (s *Service) Get(ctx context.Context, id string) (Example, error) {
	return s.repo.Get(ctx, id)
}

func (s *Service) List(ctx context.Context, params ListParams) (ListResult, error) {
	if params.Limit == 0 {
		params.Limit = 20
	}
	if params.Limit < 1 || params.Limit > 100 || params.Offset < 0 {
		return ListResult{}, apperror.Invalid("limit must be 1 to 100 and offset must be nonnegative")
	}
	items, total, err := s.repo.List(ctx, params)
	if err != nil {
		return ListResult{}, err
	}
	return ListResult{Items: items, Limit: params.Limit, Offset: params.Offset, Total: total}, nil
}

func (s *Service) Update(ctx context.Context, id, name string) (Example, error) {
	name, err := validName(name)
	if err != nil {
		return Example{}, err
	}
	return s.repo.Update(ctx, id, name, s.now().UTC())
}

func (s *Service) Delete(ctx context.Context, id string) error {
	return s.repo.Delete(ctx, id)
}

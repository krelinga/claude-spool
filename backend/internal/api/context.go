package api

import (
	"context"

	"github.com/krelinga/claude-spool/backend/internal/config"
)

func contextWithToken(ctx context.Context, tok *config.TokenConfig) context.Context {
	return context.WithValue(ctx, tokenKey, tok)
}

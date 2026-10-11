package httpapi

import (
	"context"
	"time"
)

const imageGenerationMaxRetries = 3

func waitImageGenerationRetry(ctx context.Context, attempt int) error {
	timer := time.NewTimer(time.Second << min(attempt, imageGenerationMaxRetries-1))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

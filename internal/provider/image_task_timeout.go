package provider

import (
	"context"
	"time"
)

type imageTaskDeadlineKey struct{}

// WithImageTaskTimeout fixes one budget for queueing, retries and result retrieval.
// Nested image handlers reuse the deadline instead of restarting the clock.
func WithImageTaskTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ImageTaskDeadline(ctx); ok {
		return ctx, func() {}
	}
	limited, cancel := context.WithTimeout(ctx, timeout)
	deadline, _ := limited.Deadline()
	return context.WithValue(limited, imageTaskDeadlineKey{}, deadline), cancel
}

func ImageTaskDeadline(ctx context.Context) (time.Time, bool) {
	deadline, ok := ctx.Value(imageTaskDeadlineKey{}).(time.Time)
	return deadline, ok
}

func (o *OpenAIImage) imagePollDeadline(ctx context.Context) time.Time {
	timeout := o.RequestTimeout
	if timeout <= 0 {
		timeout = openAIImageDefaultPollTimeout
	}
	deadline := time.Now().Add(timeout)
	if taskDeadline, ok := ImageTaskDeadline(ctx); ok {
		deadline = taskDeadline
	}
	if parentDeadline, ok := ctx.Deadline(); ok && parentDeadline.Before(deadline) {
		deadline = parentDeadline
	}
	return deadline
}

package httpapi

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

const defaultImageTaskTimeoutSeconds = 600

func parseImageTaskTimeoutSeconds(value any) (int, error) {
	var seconds float64
	switch v := value.(type) {
	case float64:
		seconds = v
	case int:
		seconds = float64(v)
	default:
		return 0, fmt.Errorf("image_task_timeout_secs must be an integer from 60 to 900 seconds")
	}
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds != math.Trunc(seconds) || seconds < 60 || seconds > 900 {
		return 0, fmt.Errorf("image_task_timeout_secs must be an integer from 60 to 900 seconds")
	}
	return int(seconds), nil
}

func configuredImageTaskTimeoutSeconds(settings map[string]any) int {
	seconds, err := parseImageTaskTimeoutSeconds(settings["image_task_timeout_secs"])
	if err != nil {
		return defaultImageTaskTimeoutSeconds
	}
	return seconds
}

// Settings are captured once per new request; in-flight tasks keep their deadline.
func (s *Server) imageTaskContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := provider.ImageTaskDeadline(ctx); ok {
		return ctx, func() {}
	}
	var settings map[string]any
	if s.store != nil {
		settings, _ = s.store.Config()
	}
	return provider.WithImageTaskTimeout(ctx, time.Duration(configuredImageTaskTimeoutSeconds(settings))*time.Second)
}

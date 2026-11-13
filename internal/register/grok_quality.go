package register

import "errors"

type GrokQualitySchedule struct {
	Enabled       bool `json:"enabled"`
	IntervalHours int  `json:"interval_hours"`
}

func (s *Store) GrokQualitySchedule() GrokQualitySchedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, _ := s.getLocked()["grok_quality_schedule"].(map[string]any)
	interval := positiveValue(value["interval_hours"], 24)
	if interval > 168 {
		interval = 24
	}
	return GrokQualitySchedule{Enabled: boolValue(value["enabled"], true), IntervalHours: interval}
}

func (s *Store) SetGrokQualitySchedule(config GrokQualitySchedule) error {
	if config.IntervalHours < 1 || config.IntervalHours > 168 {
		return errors.New("检测间隔应为1至168小时")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	value := s.getLocked()
	value["grok_quality_schedule"] = map[string]any{"enabled": config.Enabled, "interval_hours": config.IntervalHours}
	return writeJSON0600(s.configPath, value)
}

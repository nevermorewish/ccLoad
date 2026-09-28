package model

// ChannelMonitorSchedule is shared by the monitor editor and the daily scheduler.
type ChannelMonitorSchedule struct {
	Enabled         bool   `json:"enabled"`
	IntervalMinutes int    `json:"interval_minutes"`
	StartTime       string `json:"start_time"`
	Model           string `json:"model"`
}

type ChannelMonitorProbe struct {
	Time       JSONTime `json:"time"`
	StatusCode int      `json:"status_code"`
	Duration   float64  `json:"duration"`
	Message    string   `json:"message,omitempty"`
}

type ChannelMonitorStats struct {
	ChannelID      int64                 `json:"channel_id"`
	Model          string                `json:"model"`
	Samples        int64                 `json:"samples"`
	Successes      int64                 `json:"successes"`
	AverageLatency float64               `json:"average_latency"`
	Recent         []ChannelMonitorProbe `json:"recent"`
}

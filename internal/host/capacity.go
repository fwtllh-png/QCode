package host

const (
	defaultMaxJSONBodyBytes       int64 = 12 << 20
	defaultMaxWebSocketFrameBytes int64 = 4 << 20
	defaultMaxReplayEvents              = 10_000
	defaultMaxConnections               = 16
	defaultMaxActiveSessions            = 32
	defaultMaxIdentityBytes             = 256
	defaultMaxTraceTurns                = 128
	// A launch code is redeemed by the browser the launcher opens, or by a
	// person clicking the printed URL; it is useless once redeemed.
	defaultLaunchCodeTTLSeconds = 300
)

// Capacity centralizes the externally observable Web Host limits.
type Capacity struct {
	MaxJSONBodyBytes       int64 `json:"max_json_body_bytes"`
	MaxWebSocketFrameBytes int64 `json:"max_websocket_frame_bytes"`
	MaxReplayEvents        int   `json:"max_replay_events"`
	MaxConnections         int   `json:"max_connections"`
	MaxActiveSessions      int   `json:"max_active_sessions"`
	MaxIdentityBytes       int   `json:"max_identity_bytes"`
	MaxTraceTurns          int   `json:"max_trace_turns"`
	LaunchCodeTTLSeconds   int   `json:"launch_code_ttl_seconds"`
}

func defaultCapacity() Capacity { return Capacity{}.normalized() }

func (c Capacity) normalized() Capacity {
	if c.MaxJSONBodyBytes <= 0 {
		c.MaxJSONBodyBytes = defaultMaxJSONBodyBytes
	}
	if c.MaxWebSocketFrameBytes <= 0 {
		c.MaxWebSocketFrameBytes = defaultMaxWebSocketFrameBytes
	}
	if c.MaxReplayEvents <= 0 {
		c.MaxReplayEvents = defaultMaxReplayEvents
	}
	if c.MaxConnections <= 0 {
		c.MaxConnections = defaultMaxConnections
	}
	if c.MaxActiveSessions <= 0 {
		c.MaxActiveSessions = defaultMaxActiveSessions
	}
	if c.MaxActiveSessions > 1000 {
		c.MaxActiveSessions = 1000
	}
	if c.MaxIdentityBytes <= 0 {
		c.MaxIdentityBytes = defaultMaxIdentityBytes
	}
	if c.MaxTraceTurns <= 0 {
		c.MaxTraceTurns = defaultMaxTraceTurns
	}
	if c.LaunchCodeTTLSeconds <= 0 {
		c.LaunchCodeTTLSeconds = defaultLaunchCodeTTLSeconds
	}
	return c
}

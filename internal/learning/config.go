package learning

import (
	"fmt"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

type PermissionConfig struct {
	IncludePrivateChats bool     `json:"include_private_chats"`
	IncludeGroupChats   bool     `json:"include_group_chats"`
	ExcludedTerms       []string `json:"excluded_terms,omitempty"`
	AllowDomains        []string `json:"allow_domains,omitempty"`
	BlockDomains        []string `json:"block_domains,omitempty"`
}

// Excludes is a deterministic local hook; future topic classification also
// needs to apply the policy before selection and before any provider call.
func (p PermissionConfig) Excludes(turn ConversationTurn) bool {
	text := strings.ToLower(turn.Transcript)
	for _, term := range p.ExcludedTerms {
		if term = strings.ToLower(strings.TrimSpace(term)); term != "" && strings.Contains(text, term) {
			return true
		}
	}
	return false
}

// AllowsDomain applies a blocklist first, then an optional allowlist.
func (p PermissionConfig) AllowsDomain(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	matches := func(domains []string) bool {
		for _, domain := range domains {
			domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
			if domain != "" && (host == domain || strings.HasSuffix(host, "."+domain)) {
				return true
			}
		}
		return false
	}
	return host != "" && !matches(p.BlockDomains) && (len(p.AllowDomains) == 0 || matches(p.AllowDomains))
}

// ResearchURI accepts only explicitly approved HTTPS domains and URLs without
// query parameters, fragments, or credentials.
func (p PermissionConfig) ResearchURI(raw string) bool {
	if len(raw) > 1024 || len(p.AllowDomains) == 0 || strings.ContainsAny(raw, "?#") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.Port() != "" {
		return false
	}
	host := u.Hostname()
	_, ipErr := netip.ParseAddr(host)
	return ipErr != nil && strings.Contains(host, ".") && p.AllowsDomain(host)
}

// Budget measures upper limits, or spent units when embedded in a Run.
type Budget struct {
	Requests       int64 `json:"requests"`
	Documents      int64 `json:"documents"`
	Tokens         int64 `json:"tokens"`
	CostMicros     int64 `json:"cost_usd_micros"`
	WallTimeMillis int64 `json:"wall_time_ms"`
	Depth          int64 `json:"depth"`
	StorageBytes   int64 `json:"storage_bytes"`
}

func (b Budget) hasLimits() bool {
	return b.Requests > 0 && b.Documents > 0 && b.Tokens > 0 && b.CostMicros > 0 &&
		b.WallTimeMillis > 0 && b.Depth > 0 && b.StorageBytes > 0
}

func (b Budget) Fits(limit Budget) bool {
	return b.Requests <= limit.Requests && b.Documents <= limit.Documents &&
		b.Tokens <= limit.Tokens && b.CostMicros <= limit.CostMicros &&
		b.WallTimeMillis <= limit.WallTimeMillis && b.Depth <= limit.Depth &&
		b.StorageBytes <= limit.StorageBytes
}

type ScheduleConfig struct {
	Enabled     bool   `json:"enabled"`
	TimeZone    string `json:"timezone"`
	WindowStart string `json:"window_start"`
	WindowEnd   string `json:"window_end"`
	IdleMinutes int    `json:"idle_minutes"`
}

type Config struct {
	SchemaVersion string                `json:"schema_version"`
	Enabled       bool                  `json:"enabled"`
	Schedule      ScheduleConfig        `json:"schedule"`
	Permissions   PermissionConfig      `json:"permissions"`
	SourcePolicy  *ResearchSourcePolicy `json:"source_policy,omitempty"`
	Selection     SelectionConfig       `json:"selection"`
	Recall        RecallConfig          `json:"recall,omitzero"`
	GlobalDaily   Budget                `json:"global_daily"`
	PerRun        Budget                `json:"per_run"`
	PerQuestion   Budget                `json:"per_question"`
}

type RecallConfig struct {
	ChatEnabled  bool `json:"chat_enabled,omitempty"`
	AuditEnabled bool `json:"audit_enabled,omitempty"`
}

// Defaults keep both learning and scheduling opt-in. Budget figures are
// conservative development defaults, expressed in API accounting units.
func DefaultConfig() Config {
	return Config{
		SchemaVersion: SchemaVersion,
		Selection:     SelectionConfig{Conversation: 70, Gap: 20, Association: 10, MaxTopics: 5, MaxDepth: 2},
		Schedule:      ScheduleConfig{TimeZone: "UTC", WindowStart: "02:00", WindowEnd: "05:00", IdleMinutes: 30},
		GlobalDaily:   Budget{Requests: 30, Documents: 16, Tokens: 40000, CostMicros: 200000, WallTimeMillis: 600000, Depth: 2, StorageBytes: 2 << 20},
		PerRun:        Budget{Requests: 12, Documents: 8, Tokens: 16000, CostMicros: 100000, WallTimeMillis: 300000, Depth: 2, StorageBytes: 1 << 20},
		PerQuestion:   Budget{Requests: 8, Documents: 4, Tokens: 8000, CostMicros: 50000, WallTimeMillis: 180000, Depth: 2, StorageBytes: 512 << 10},
	}
}

func (c Config) Validate() error {
	if c.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported learning config schema version")
	}
	if c.Schedule.TimeZone == "" || c.Schedule.TimeZone == "Local" {
		return fmt.Errorf("learning schedule requires an explicit IANA time zone")
	}
	if _, err := time.LoadLocation(c.Schedule.TimeZone); err != nil {
		return fmt.Errorf("invalid learning time zone")
	}
	start, startErr := time.Parse("15:04", c.Schedule.WindowStart)
	end, endErr := time.Parse("15:04", c.Schedule.WindowEnd)
	if startErr != nil || endErr != nil || start.Format("15:04") != c.Schedule.WindowStart ||
		end.Format("15:04") != c.Schedule.WindowEnd || start.Equal(end) || c.Schedule.IdleMinutes < 0 {
		return fmt.Errorf("invalid learning schedule window or idle threshold")
	}
	if c.Schedule.Enabled && !c.Enabled {
		return fmt.Errorf("learning schedule requires learning to be enabled")
	}
	if !c.GlobalDaily.hasLimits() || !c.PerRun.hasLimits() || !c.PerQuestion.hasLimits() ||
		!c.PerQuestion.Fits(c.PerRun) || !c.PerRun.Fits(c.GlobalDaily) {
		return fmt.Errorf("learning budgets must be positive and nested")
	}
	if err := c.SourcePolicy.validate(); err != nil {
		return err
	}
	return c.Selection.Validate()
}

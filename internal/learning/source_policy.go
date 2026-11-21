package learning

import (
	"fmt"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// ResearchSourcePolicy admits sources based on their declared publication date.
// An exact host rule can override the default age or authorize a single source.
type ResearchSourcePolicy struct {
	MaxAgeDays int                         `json:"max_age_days,omitempty"`
	Domains    map[string]DomainSourceRule `json:"domains,omitempty"`
}

type DomainSourceRule struct {
	MaxAgeDays      *int `json:"max_age_days,omitempty"`
	SingleAuthority bool `json:"single_authority,omitempty"`
}

func (p *ResearchSourcePolicy) validate() error {
	if p == nil {
		return nil
	}
	const maxAgeDays = 36500
	if p.MaxAgeDays < 0 || p.MaxAgeDays > maxAgeDays {
		return fmt.Errorf("source max_age_days must be between 0 and %d", maxAgeDays)
	}
	for host, rule := range p.Domains {
		if !validSourcePolicyHost(host) {
			return fmt.Errorf("source domain rules require exact lowercase DNS hostnames")
		}
		if age := rule.MaxAgeDays; age != nil && (*age < 0 || *age > maxAgeDays) {
			return fmt.Errorf("source domain max_age_days must be between 0 and %d", maxAgeDays)
		}
	}
	return nil
}

func validSourcePolicyHost(host string) bool {
	if len(host) > 253 || !strings.Contains(host, ".") {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return false
	}
	for part := range strings.SplitSeq(host, ".") {
		if part == "" || len(part) > 63 || part[0] == '-' || part[len(part)-1] == '-' {
			return false
		}
		for i := range len(part) {
			b := part[i]
			if b != '-' && !(b >= 'a' && b <= 'z') && !(b >= '0' && b <= '9') {
				return false
			}
		}
	}
	return true
}

func (p *ResearchSourcePolicy) domainRule(uri string) DomainSourceRule {
	if p == nil || len(p.Domains) == 0 {
		return DomainSourceRule{}
	}
	u, err := url.Parse(uri)
	if err != nil {
		return DomainSourceRule{}
	}
	return p.Domains[strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))]
}

func (p *ResearchSourcePolicy) Accepts(source Source, now time.Time) bool {
	if p == nil {
		return true
	}
	age := p.MaxAgeDays
	if rule := p.domainRule(source.CanonicalURI); rule.MaxAgeDays != nil {
		age = *rule.MaxAgeDays
	}
	if age == 0 {
		return true
	}
	published := source.PublishedAt
	cutoff := now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -age)
	return published != nil && !published.IsZero() && !published.After(now) &&
		!published.After(source.FetchedAt) && !published.Before(cutoff)
}

func (e EvidenceRequirements) HasRequiredSources(refs []ResearchReference) bool {
	return len(refs) >= e.MinIndependentSources ||
		(e.AllowSingleAuthority && len(refs) == 1 && refs[0].Evidence != nil &&
			e.SourcePolicy.domainRule(refs[0].Source.CanonicalURI).SingleAuthority)
}

func (c Config) ResearchRequirements(minimum int) EvidenceRequirements {
	requirements := EvidenceRequirements{MinIndependentSources: minimum, SourcePolicy: c.SourcePolicy}
	if c.SourcePolicy != nil && minimum > 1 {
		for host, rule := range c.SourcePolicy.Domains {
			if rule.SingleAuthority && c.Permissions.ResearchURI("https://"+host) {
				requirements.AllowSingleAuthority = true
				break
			}
		}
	}
	return requirements
}

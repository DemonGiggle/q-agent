package learning

import (
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"
)

const SchemaVersion = "1.0"

type Record struct {
	SchemaVersion string    `json:"schema_version"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (r Record) valid() bool {
	return r.SchemaVersion == SchemaVersion && !r.CreatedAt.IsZero() && !r.UpdatedAt.Before(r.CreatedAt)
}

type Topic struct {
	Record
	ID           string   `json:"topic_id"`
	Title        string   `json:"title"`
	TaxonomyPath []string `json:"taxonomy_path"`
	Aliases      []string `json:"aliases"`
	Status       string   `json:"status"`
}

type Score struct {
	Interest    float64 `json:"interest"`
	Gap         float64 `json:"gap"`
	Utility     float64 `json:"utility"`
	Novelty     float64 `json:"novelty"`
	Feasibility float64 `json:"feasibility"`
	Penalty     float64 `json:"penalty"`
	Final       float64 `json:"final"`
}

type Question struct {
	Record
	ID            string     `json:"question_id"`
	Text          string     `json:"text"`
	OriginType    string     `json:"origin_type"`
	OriginRefs    []string   `json:"origin_refs"`
	Reason        string     `json:"reason"`
	ExpectedValue string     `json:"expected_value"`
	TopicIDs      []string   `json:"topic_ids"`
	Score         *Score     `json:"score_breakdown,omitempty"`
	Priority      int        `json:"priority"`
	Status        string     `json:"status"`
	Depth         int64      `json:"depth"`
	ParentID      string     `json:"parent_id,omitempty"`
	CooldownUntil *time.Time `json:"cooldown_until,omitempty"`
}

func (q Question) Validate() error {
	if !q.Record.valid() || q.ID == "" || strings.TrimSpace(q.Text) == "" || q.Depth < 0 ||
		!oneOf(q.Status, "candidate", "queued", "running", "resolved", "unresolved", "dismissed") ||
		!oneOf(q.OriginType, "manual", "conversation", "gap", "association") ||
		(q.OriginType != "manual" && len(q.OriginRefs) == 0) {
		return fmt.Errorf("invalid learning question")
	}
	if q.Score != nil {
		for _, n := range []float64{q.Score.Interest, q.Score.Gap, q.Score.Utility, q.Score.Novelty, q.Score.Feasibility, q.Score.Penalty, q.Score.Final} {
			if math.IsNaN(n) || n < 0 || n > 1 {
				return fmt.Errorf("learning scores must be between zero and one")
			}
		}
	}
	return nil
}

type EvidenceRequirements struct {
	MinIndependentSources int                   `json:"min_independent_sources"`
	AllowSingleAuthority  bool                  `json:"allow_single_authority"`
	SourcePolicy          *ResearchSourcePolicy `json:"source_policy,omitempty"`
}

type Plan struct {
	Record
	ID                   string               `json:"plan_id"`
	QuestionID           string               `json:"question_id"`
	Goal                 string               `json:"goal"`
	Subquestions         []string             `json:"subquestions"`
	Steps                []string             `json:"steps"`
	EvidenceRequirements EvidenceRequirements `json:"evidence_requirements"`
	AllowedSourceClasses []string             `json:"allowed_source_classes"`
	StopConditions       []string             `json:"stop_conditions"`
	Budget               Budget               `json:"budget"`
	Version              uint64               `json:"version"`
}

func (p Plan) Validate() error {
	if !p.Record.valid() || p.ID == "" || p.QuestionID == "" || strings.TrimSpace(p.Goal) == "" ||
		len(p.Steps) == 0 || len(p.StopConditions) == 0 || len(p.AllowedSourceClasses) == 0 ||
		p.Version == 0 || p.EvidenceRequirements.MinIndependentSources < 1 || !p.Budget.hasLimits() {
		return fmt.Errorf("invalid learning plan")
	}
	return nil
}

type RunState string

const (
	RunQueued          RunState = "QUEUED"
	RunPlanned         RunState = "PLANNED"
	RunRunning         RunState = "RUNNING"
	RunValidating      RunState = "VALIDATING"
	RunCommitting      RunState = "COMMITTING"
	RunCompleted       RunState = "COMPLETED"
	RunPaused          RunState = "PAUSED"
	RunFailed          RunState = "FAILED"
	RunBudgetExhausted RunState = "BUDGET_EXHAUSTED"
	RunCancelled       RunState = "CANCELLED"
	RunUnresolved      RunState = "UNRESOLVED"
)

type ResearchReference struct {
	Source   Source    `json:"source"`
	Evidence *Evidence `json:"evidence,omitempty"`
}

type Checkpoint struct {
	ResearchCandidates []string `json:"research_candidates,omitempty"`
	ResearchAttempt    int      `json:"research_attempt,omitempty"`
	// ResearchRequests is the reserved request count at the last saved HTTP outcome.
	ResearchRequests      int64               `json:"research_requests,omitempty"`
	ResearchReferences    []ResearchReference `json:"research_references,omitempty"`
	ResearchURI           string              `json:"research_uri,omitempty"`
	ResearchSource        *Source             `json:"research_source,omitempty"`
	ResearchEvidence      *Evidence           `json:"research_evidence,omitempty"`
	ResearchSummary       string              `json:"research_summary,omitempty"`
	ResearchSummaryStatus string              `json:"research_summary_status,omitempty"`
	ResearchClaim         *Claim              `json:"research_claim,omitempty"`
	PublishedClaimID      string              `json:"published_claim_id,omitempty"`
	Version               uint64              `json:"version"`
	OpenQuestions         []string            `json:"open_questions"`
	Fingerprints          []string            `json:"fingerprints"`
	Spent                 Budget              `json:"spent_budget"`
	OperationID           string              `json:"operation_id,omitempty"`
	NextIntent            string              `json:"next_intent"`
}

type Run struct {
	Record
	ID            string      `json:"run_id"`
	PlanID        string      `json:"plan_id"`
	State         RunState    `json:"state"`
	Checkpoint    *Checkpoint `json:"checkpoint,omitempty"`
	Spent         Budget      `json:"spent_budget"`
	StartedAt     *time.Time  `json:"started_at,omitempty"`
	EndedAt       *time.Time  `json:"ended_at,omitempty"`
	StopReason    string      `json:"stop_reason,omitempty"`
	CoverageScore float64     `json:"coverage_score"`
}

type Source struct {
	Record
	ID                string       `json:"source_id"`
	CanonicalURI      string       `json:"canonical_uri"`
	SyndicationOrigin string       `json:"syndication_origin,omitempty"`
	Title             string       `json:"title"`
	Publisher         string       `json:"publisher,omitempty"`
	PublishedAt       *time.Time   `json:"published_at,omitempty"`
	FetchedAt         time.Time    `json:"fetched_at"`
	ContentHash       string       `json:"content_hash"`
	License           string       `json:"license,omitempty"`
	TrustClass        string       `json:"trust_class"`
	Check             *SourceCheck `json:"revalidation,omitempty"`
}

type SourceCheck struct {
	At         time.Time  `json:"checked_at"`
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
	Result     string     `json:"result"`
}

// Available reports whether the stored version can still support a current claim.
func (s Source) Available() bool {
	return s.Check == nil || oneOf(s.Check.Result, "unchanged", "inconclusive")
}

func (s Source) LastVerifiedAt() time.Time {
	if s.Check != nil && s.Check.VerifiedAt != nil {
		return *s.Check.VerifiedAt
	}
	return s.FetchedAt
}

func (s Source) Validate() error {
	uri, err := url.Parse(s.CanonicalURI)
	if !s.Record.valid() || s.ID == "" || s.FetchedAt.IsZero() || s.ContentHash == "" ||
		err != nil || !oneOf(uri.Scheme, "http", "https") || uri.Hostname() == "" || uri.User != nil {
		return fmt.Errorf("invalid learning source")
	}
	if c := s.Check; c != nil && (c.At.Before(s.FetchedAt) ||
		!oneOf(c.Result, "unchanged", "inconclusive", "changed", "gone", "withdrawn", "aged") ||
		(c.Result == "unchanged" && (c.VerifiedAt == nil || !c.VerifiedAt.Equal(c.At))) ||
		(c.VerifiedAt != nil && (c.VerifiedAt.After(c.At) || c.VerifiedAt.Before(s.FetchedAt)))) {
		return fmt.Errorf("invalid source revalidation")
	}
	return nil
}

// Locator anchors a claim's evidence in a particular fetched representation.
type Locator struct {
	Selector string `json:"selector"`
	Start    int    `json:"start"`
	End      int    `json:"end"`
}

type Evidence struct {
	Record
	ID               string    `json:"evidence_id"`
	SourceID         string    `json:"source_id"`
	Locator          Locator   `json:"locator"`
	ExcerptOrSummary string    `json:"excerpt_or_summary"`
	ExtractedAt      time.Time `json:"extracted_at"`
}

// EvidenceLink preserves the role of the same excerpt for each claim.
type EvidenceLink struct {
	EvidenceID string `json:"evidence_id"`
	Relation   string `json:"relation"`
}

type Claim struct {
	Record
	ID               string            `json:"claim_id"`
	Subject          string            `json:"subject"`
	Predicate        string            `json:"predicate"`
	Object           string            `json:"object"`
	Qualifiers       map[string]string `json:"qualifiers"`
	Status           string            `json:"status"`
	Confidence       float64           `json:"confidence"`
	ConfidenceReason string            `json:"confidence_reason,omitempty"`
	Aliases          []string          `json:"aliases,omitempty"`
	SupersededBy     string            `json:"superseded_by,omitempty"`
	Evidence         []EvidenceLink    `json:"evidence"`
	ValidFrom        *time.Time        `json:"valid_from,omitempty"`
	ValidTo          *time.Time        `json:"valid_to,omitempty"`
	RevisedAt        time.Time         `json:"revised_at"`
	Revision         uint64            `json:"revision"`
}

func (c Claim) Validate() error {
	if !c.Record.valid() || c.ID == "" || strings.TrimSpace(c.Subject) == "" ||
		strings.TrimSpace(c.Predicate) == "" || strings.TrimSpace(c.Object) == "" ||
		c.Revision == 0 || c.RevisedAt.IsZero() || math.IsNaN(c.Confidence) || c.Confidence < 0 || c.Confidence > 1 ||
		!oneOf(c.Status, "hypothesis", "unresolved", "supported", "disputed", "stale", "superseded") ||
		(c.ValidFrom != nil && c.ValidTo != nil && c.ValidTo.Before(*c.ValidFrom)) {
		return fmt.Errorf("invalid learning claim")
	}
	supports := false
	for _, link := range c.Evidence {
		if link.EvidenceID == "" || !oneOf(link.Relation, "supports", "refutes", "context") {
			return fmt.Errorf("invalid claim evidence link")
		}
		supports = supports || link.Relation == "supports"
	}
	if c.Status == "supported" && !supports {
		return fmt.Errorf("supported claim requires supporting evidence")
	}
	if len(c.Evidence) == 0 && !oneOf(c.Status, "hypothesis", "unresolved") {
		return fmt.Errorf("claim without evidence must remain a hypothesis or unresolved")
	}
	return nil
}

type Note struct {
	Record
	ID          string     `json:"note_id"`
	Title       string     `json:"title"`
	TopicIDs    []string   `json:"topic_ids"`
	Summary     string     `json:"summary"`
	BodyRef     string     `json:"body_ref"`
	ClaimIDs    []string   `json:"claim_ids"`
	SourceIDs   []string   `json:"source_ids"`
	EvidenceIDs []string   `json:"evidence_ids,omitempty"`
	Revision    uint64     `json:"revision"`
	ArchivedAt  *time.Time `json:"archived_at,omitempty"`
}

type Relation struct {
	Record
	ID            string            `json:"edge_id"`
	ClaimID       string            `json:"claim_id,omitempty"`
	ClaimRevision uint64            `json:"claim_revision,omitempty"`
	ValidFrom     *time.Time        `json:"valid_from,omitempty"`
	ValidTo       *time.Time        `json:"valid_to,omitempty"`
	FromID        string            `json:"from_id"`
	Predicate     string            `json:"predicate"`
	ToID          string            `json:"to_id"`
	Qualifiers    map[string]string `json:"qualifiers"`
	EvidenceIDs   []string          `json:"evidence_ids"`
}

type RevisionEvent struct {
	Record
	ID          string    `json:"event_id"`
	EntityType  string    `json:"entity_type"`
	EntityID    string    `json:"entity_id"`
	OldRevision uint64    `json:"old_revision,omitempty"`
	NewRevision uint64    `json:"new_revision"`
	Reason      string    `json:"reason"`
	EvidenceIDs []string  `json:"evidence_ids"`
	OccurredAt  time.Time `json:"occurred_at"`
	Decision    string    `json:"decision,omitempty"`
	Before      *Claim    `json:"before,omitempty"`
}

type RecallEvent struct {
	Record
	ID               string    `json:"event_id"`
	QueryRef         string    `json:"query_ref,omitempty"`
	RetrievedNoteIDs []string  `json:"retrieved_note_ids"`
	UsedClaimIDs     []string  `json:"used_claim_ids"`
	Relevance        float64   `json:"relevance"`
	Feedback         string    `json:"feedback,omitempty"`
	OccurredAt       time.Time `json:"occurred_at"`
}

type FrontierEntry struct {
	Question          Question             `json:"question"`
	DependsOn         []string             `json:"depends_on"`
	Transitions       []FrontierTransition `json:"transitions"`
	RunID             string               `json:"run_id,omitempty"`
	OperatorSelection string               `json:"operator_selection,omitempty"`
}

type FrontierTransition struct {
	At     time.Time `json:"at"`
	Status string    `json:"status"`
	Reason string    `json:"reason"`
}

func oneOf(value string, allowed ...string) bool {
	for _, entry := range allowed {
		if entry == value {
			return true
		}
	}
	return false
}

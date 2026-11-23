package learning

import (
	"testing"
	"time"
)

func TestLearningValidation(t *testing.T) {
	now := time.Now()
	record := Record{SchemaVersion: SchemaVersion, CreatedAt: now, UpdatedAt: now}
	q := Question{Record: record, ID: "q", Text: "example?", Status: "queued", OriginType: "conversation"}
	if err := q.Validate(); err == nil {
		t.Fatal("accepted conversation question without origin")
	}
	q.OriginRefs = []string{"origin"}
	if err := q.Validate(); err != nil {
		t.Fatal(err)
	}
	c := Claim{Record: record, ID: "c", Subject: "subject", Predicate: "predicate", Object: "object", Status: "supported", RevisedAt: now, Revision: 1}
	if err := c.Validate(); err == nil {
		t.Fatal("accepted supported claim without evidence")
	}
	c.Evidence = []EvidenceLink{{EvidenceID: "e", Relation: "refutes"}}
	if err := c.Validate(); err == nil {
		t.Fatal("accepted supported claim with only contrary evidence")
	}
	c.Status = "disputed"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	open := PermissionConfig{BlockDomains: []string{"blocked.example.org"}}
	if !open.ResearchURI("https://example.org/doc") || open.ResearchURI("https://blocked.example.org/doc") {
		t.Fatal("open ResearchURI did not allow non-blocked HTTPS domain")
	}
	restricted := PermissionConfig{AllowDomains: []string{"example.org"}}
	if !restricted.ResearchURI("https://docs.example.org/doc") || restricted.ResearchURI("https://other.org/doc") {
		t.Fatal("restricted ResearchURI did not enforce AllowDomains")
	}
}

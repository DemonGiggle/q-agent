package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"q-agent/internal/learning"
)

type LearningMetrics struct {
	From                 time.Time                  `json:"from"`
	Until                time.Time                  `json:"until"`
	Events               map[learning.EventType]int `json:"events"`
	ResearchOutcomes     map[learning.RunState]int  `json:"research_outcomes"`
	StopReasons          map[string]int             `json:"stop_reasons"`
	CommitOutcomes       map[string]int             `json:"commit_outcomes"`
	TerminalReservations learning.Budget            `json:"terminal_reservations"`
	RecallNoteRefs       int                        `json:"recall_note_refs"`
	RecallClaimRefs      int                        `json:"recall_claim_refs"`
	InvalidFiles         int                        `json:"invalid_files"`
}

// Metrics reads immutable, uniquely named audit events on demand. Copies of
// recall feedback, exports and incomplete temporary writes do not count.
func (a LearningAudit) Metrics(ctx context.Context, from, until time.Time) (LearningMetrics, error) {
	report := LearningMetrics{
		From: from.UTC(), Until: until.UTC(), Events: make(map[learning.EventType]int),
		ResearchOutcomes: make(map[learning.RunState]int), StopReasons: make(map[string]int),
		CommitOutcomes: make(map[string]int),
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if from.IsZero() || !from.Before(until) {
		return report, fmt.Errorf("invalid learning metrics interval")
	}
	dir, err := os.Open(a.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return report, nil
	}
	if err != nil {
		return report, err
	}
	defer dir.Close()
	max := learning.Budget{
		Requests: math.MaxInt64, Documents: math.MaxInt64, Tokens: math.MaxInt64, CostMicros: math.MaxInt64,
		WallTimeMillis: math.MaxInt64, Depth: math.MaxInt64, StorageBytes: math.MaxInt64,
	}
	for {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		files, dirErr := dir.ReadDir(128)
		for _, entry := range files {
			if err := ctx.Err(); err != nil {
				return report, err
			}
			id, jsonFile := strings.CutSuffix(entry.Name(), ".json")
			if !jsonFile || !auditID.MatchString(id) || !entry.Type().IsRegular() {
				continue
			}
			event, ok := readMetricEvent(filepath.Join(a.Dir, entry.Name()), id)
			if !ok {
				report.InvalidFiles++
				continue
			}
			if event.At.Before(from) || !event.At.Before(until) {
				continue
			}
			report.Events[event.Type]++
			switch event.Type {
			case learning.ResearchTransition:
				state := learning.RunState(event.Outcome)
				if terminalLearningRun(state) {
					report.ResearchOutcomes[state]++
					if event.Reason != "" {
						report.StopReasons[event.Reason]++
					}
					if event.Spent != nil {
						spent, ok := report.TerminalReservations.Reserve(*event.Spent, max)
						if !ok {
							return report, fmt.Errorf("invalid cumulative learning reservations")
						}
						report.TerminalReservations = spent
					}
				}
			case learning.KnowledgeCommitted:
				report.CommitOutcomes[event.Outcome]++
			case learning.RecallUsed:
				report.RecallNoteRefs += len(event.ObjectRefs)
				report.RecallClaimRefs += len(event.ClaimRefs)
			}
		}
		if dirErr != nil {
			if errors.Is(dirErr, io.EOF) {
				return report, nil
			}
			return report, dirErr
		}
	}
}

func readMetricEvent(path, id string) (learning.Event, bool) {
	var event learning.Event
	f, err := os.Open(path)
	if err != nil {
		return event, false
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 4097))
	ok := err == nil && len(raw) <= 4096 && json.Unmarshal(raw, &event) == nil &&
		event.ID == id && validateAuditEvent(event) == nil
	return event, ok
}

# ACA acceptance and baseline

Run the deterministic ACA fixtures with `make learning-acceptance`. Standard
CI runs the same Go tests on both supported Go tracks and repeats them under
the Linux race detector. The fixtures use local state and stub providers.

| ID | CI test | Verified behavior |
| --- | --- | --- |
| AC-01 | `runtime/TestLearningNightResearchReachesChatRecall` | A recent authorized discussion creates a related follow-up with an origin reference. A separately approved query runs in the idle window. |
| AC-02 | `learning/TestRankQuestionAndConversationDedup`, `state/TestLearningPublicFrontierHandoff` | Equivalent questions reuse their candidate. Repeating an enqueue reuses its run ID. A new revision of the same proposition stays suppressed during cooldown. |
| AC-03 | `state/TestLearningWorker/fetch_failed`, `state/TestLearningWorker/tampered_body` | Discovery without a verified fetch or an intact body leaves the run unresolved and publishes no cited result. |
| AC-04 | `state/TestLearningClaimEvolutionAndHistory`, `state/TestLearningTelemetryResumesAndCountsClaimOutcomes` | Conflicting evidence survives in a disputed Claim with its revision history and sources. |
| AC-05 | `state/TestLearningProviderBudgetSurvivesRestart`, `state/TestLearningWorker/budget_stop` | The exhausted reservation is rejected before a provider call and leaves a readable checkpoint. |
| AC-06 | `state/TestLearningClaimDraftAndRecovery`, `state/TestLearningTelemetryResumesAndCountsClaimOutcomes` | Restart after a durable commit finishes the same run and emits stable event IDs without another Claim revision. |
| AC-07 | `runtime/TestLearningRecallInInteractiveChat`, `runtime/TestLearningNightResearchReachesChatRecall` | Relevant notes arrive with their URLs, dates and locators. Unrelated prompts receive no research context. |
| AC-08 | `llm/TestLearningOllamaDraftUsesOnlyOrderedExcerpts`, `llm/TestLearningOllamaRejectsUnsolicitedToolCall`, `runtime/TestLearningRecallInInteractiveChat` | References reach the model as data. Tool-call output is refused in research and cited Telegram replies have no executable tools. |
| AC-09 | `q-agent/TestLearningPrivateFrontierLifecycle` | Authorized deletion clears the scoped record and controlled copies and returns a replayable receipt. Recent history stays clear after reopening. |
| AC-10 | `state/TestLearningFreshnessAndAssociation` | Changed, gone or withdrawn sources identify and downgrade affected Claims while preserving historical evidence. |
| AC-11 | `learning/TestRankNoteKeywords`, `runtime/TestLearningNightResearchReachesChatRecall` | An ordinary keyword lookup retrieves a cited note with the built-in JSON store. |
| AC-12 | `state/TestLearningCancelActiveWorker` | Cancelling an active scheduled run interrupts its provider, clears its approval and lease, and publishes no knowledge. |
| AC-13 | `state/TestLearningPublicFrontierHandoff`, `state/TestLearningClaimDraftAndRecovery` | A chat lead and insufficient model evidence cannot create or promote a supported Claim. |
| AC-14 | `q-agent/TestLearningFrontierUsesOnlyAuthorizedTopic`, `state/TestLearningNightRejectsNewlyExcludedQuestion` | The next selection applies updated ratios and exclusions. A newly excluded scheduled query is cancelled before spending. |
| AC-15 | `state/TestLearningConcurrentResearchCommitRecovery`, `state/TestLearningClaimEvolutionAndHistory` | Competing commits converge without losing notes or receipts. Stale concurrent claim edits cannot overwrite a committed revision. |

## M1 exit fixture

The runtime receives the same question and conversation in both arms. The
enabled arm can retrieve the overnight note under the existing context
limit. The scheduled research reserves five HTTP requests and two
documents. The cited context is checked at the provider boundary.

| Measure | Recall off | Recall on |
| --- | ---: | ---: |
| Contextual research notes | 0 | 1 |
| Verifiable source excerpts available to the answer | 0 | 2 |

The research run issued one stubbed search and two stubbed document
fetches. It reserved five HTTP requests including robots checks, and zero
LLM tokens during research.

## M2 exit fixture

The repeated question has two independent, contradictory fixture sources.
The first execution is interrupted after committing and resumes from disk.
The second execution uses the same sources and proposal at the same limits.

| Measure | Result |
| --- | ---: |
| Completed runs | 2 |
| Verified excerpts counted across both runs | 4 |
| Newly retained Claims | 1 |
| Existing Claims reused | 1 |
| Preserved disputed Claims | 1 |
| Durable Claim revisions | 1 |
| Reserved HTTP and model requests | 12 |
| Reserved documents | 4 |
| Reserved model tokens | 4,864 |

On the same recall query the disabled arm receives zero Claim records.
The enabled arm receives the disputed Claim and both cited excerpts. These
measurements describe fixture behavior. Human ratings of factual accuracy,
relevance and usefulness are pending real provider runs.

## Paired answer review

Use the same public questions, fresh conversations, model, sampling settings
and answer budget in both runs. Keep external tool use at zero and record
research reservations separately. Include answerable questions, disputed
questions and unrelated controls. Randomize which arm is labelled A per
question and give the rater only the question, A and B, and independent
ground truth. Keep the assignment key and budgets with the coordinator
until scoring is complete.

| Question ID | Accuracy A / B | Citation URL and locator A / B | Uncertainty or conflict A / B | Helpful context A / B | Unrelated context A / B | Preference |
| --- | --- | --- | --- | --- | --- | --- |
| _(one row per pair)_ | | | | | | |

Score accuracy, citations, uncertainty and helpfulness from 0 to 2, with
0 for missing or wrong, 1 for partial and 2 for fully correct or helpful.
Record unrelated context separately as present or absent and use
A, B or tie for preference. Record the number of valid pairs, per-arm
scores, wins/ties/losses and reserved requests/tokens. Divide total
reservations by human-accepted Claims or usable answers for the relevant
efficiency metric. Treat zero accepted results as an undefined ratio.

package selfhost

import (
	"net/http/httptest"
	"path/filepath"
	"testing"

	"facet/internal/compile"
	"facet/runtime"
)

// fabric_controller_core.fct ports the rest of fabric-controller
// (decision.rs, validation.rs, replica.rs, execution.rs, controller.rs).
// Every expectation below is taken from the Rust source: the nine
// `controller::tests` (inputs and asserted outputs translated one to one),
// the `impl Display` format strings in validation.rs, ControllerPolicy's
// defaults (policy.rs), and hand traces of the branches those tests leave
// uncovered. Each scenario proc returns one `|`-joined summary; a mismatch
// in any field is a behavioural divergence from the Rust crate.

func loadFabricControllerCoreApp(t *testing.T) *httptest.Server {
	t.Helper()
	g, err := compile.File(filepath.Join("fabric_controller_core.fct"))
	if err != nil {
		t.Fatalf("compile selfhost/fabric_controller_core.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestFabricControllerCore(t *testing.T) {
	ts := loadFabricControllerCoreApp(t)
	d := postJSON(t, ts, "runFabricControllerCoreDemo")

	// The pressure score behind S7/S8 (fabric-workload's calculate_pressure,
	// already ported in fabric_telemetry.fct): cpu*0.35 + mem*0.20 +
	// clamp(queue/10_000)*0.25 + clamp(worst_latency/100_000)*0.20. With the
	// tests' profile() (1000us latencies, queue 0): profile(0.95) = 0.5245,
	// profile(0.2) = 0.112, so realized_gain = 0.4125 (x10000 = 4125) and the
	// verdict is Improved (> noise floor 0.02); profile(0.9) against itself is
	// 0.0 -> Unchanged.
	want := map[string]string{
		// S1: a_stale_decision_is_refused_with_its_evidence — age 99_000 > 15_000.
		"demoStaleDecisionResult": "false|StaleDecision|99000|decision is stale: observed at 1000, now 100000 (99000ms old, limit 15000ms)",
		// S2: a_decision_computed_against_an_older_fleet_is_refused.
		"demoOlderFleetResult": "TopologySuperseded|0|1|decision was computed against topology generation 0; the fleet is now at 1",
		// S3: an_unhealthy_destination_is_refused.
		"demoUnhealthyDestinationResult": "DestinationUnhealthy|db-b|degraded|destination 'db-b' is degraded and may not receive placements",
		// S4: a_saturated_destination_is_refused_even_with_headroom — 0.95 > 0.85.
		"demoSaturatedDestinationResult": "DestinationSaturated|destination 'db-b' is saturated at 0.950 utilization (limit 0.850)",
		// S5: two_actions_cannot_run_on_one_target — first admitted as action-1.
		"demoTwoActionsOneTargetResult": "true|1|false|ConflictingAction|action-1 (move) is already executing on shard 7 (3,4)|1|stub",
		// S6: a_plan_that_would_drop_the_last_copy_is_refused — caught as RemovesUnheldCopy.
		"demoDropLastCopyResult": "RemovesUnheldCopy|plan removes a copy of shard 7 (3,4) from 'db-b', which holds none",
		// S7: an_executed_action_is_not_complete_until_it_is_measured.
		// Commit lands on the 5th tick (6_000); after 16 ticks now = 17_000,
		// so elapsed 11_000 < settle 30_000; settled at 77_000.
		"demoExecutedNotCompleteUntilMeasuredResult": "awaiting-measurement|4|true|db-a|db-b|us-west|1|true|TooSoon|action-1 committed 11000ms ago; the system settles for 30000ms before a measurement counts|improved|4125|1|true|1000|77000|0",
		// S8: an_action_that_helped_nothing_is_reported_as_such.
		"demoHelpedNothingResult": "unchanged|0|false|false",
		// S9: losing_the_destination_mid_flight_tears_the_action_down — plus
		// node loads 1/1, target held by action-1, shard busy with action-1,
		// no shard-wide holder, then everything released.
		"demoLosingDestinationResult": "1|1|true|1|false|running|rolled-back|false|NodeLost|node 'db-b' became unreachable mid-flight|0|0|rolled-back|true|node 'db-b' became unreachable mid-flight",
		// S10: validate's pre-fleet refusals; score 1.0-0.3=0.700, 0.2-0.3=-0.100.
		"demoRefusalsBeforeTheFleetResult": "NoActionRequested|decision requests no action|InvalidCoordinate|coordinate (12,0) is outside the 12x13 grid|BelowExecutionThreshold|decision below execution threshold: score 0.700, confidence 0.500 (minimum 0.800)|BelowExecutionThreshold|decision below execution threshold: score -0.100, confidence 0.900 (minimum 0.800)|DecisionFromTheFuture|decision claims observation at 2000, ahead of the control plane clock at 1000|UnknownPlacement|no placement is recorded for shard 8 (3,4)",
		// S11: mechanism gate and destination resolution.
		"demoNoMechanismAndDestinationsResult": "NoMechanism|no registered mechanism implements 'move'|DestinationIsSource|destination 'db-a' already holds the target|ColocationTargetMissing|colocation target shard 7 (5,5) is not placed|UnknownDestination|destination 'db-z' is not a registered node|PlanRejected|plan is infeasible: stub needs a named destination|DestinationAtCapacity|destination 'db-b' is at capacity: 10/10 placements",
		// S12: phase timeout — Prepare began at 2_000, polled at 62_001.
		"demoPhaseTimeoutResult": "running|prepare|rolled-back|PhaseTimeout|phase 'prepare' ran 60001ms without completing (limit 60000ms)|rolled-back|phase 'prepare' ran 60001ms without completing (limit 60000ms)",
		// S13: abort after cutover — 4 ticks leave Cleanup running with 3
		// phases done; the stub's rollback is Irreversible; ledger follows.
		"demoAbortAfterCutoverResult": "running|cleanup|3|true|failed|Irreversible|authority already moved|1|true|false|failed|abort requested|true|1|failed",
		// S14: measurement deadline — 300_000 after the 6_000 commit.
		"demoMeasurementDeadlineResult": "awaiting-measurement|1|awaiting-measurement|completed|not-measured|false|0|0|NotAwaitingMeasurement|action-1 is completed, not awaiting measurement",
		// S15: NodeBusy at max_actions_per_node = 2, refused on the source.
		"demoNodeBusyResult": "1|2|NodeBusy|node 'db-a' already has 2 action(s) in flight (limit 2)|running,running|2|2|2|3|2",
		// S16: ReplicaLedger.
		"demoReplicaLedgerResult": "3|true|1|db-a,db-b,db-c|3|2|0|2|db-a|db-b|1|true|1|shard 7 (3,4),shard 7 (5,5),shard 7 (6,6)",
		// S17: abort on request.
		"demoAbortRequestedResult": "rolled-back|abort requested|Restored|rolled-back|false|0",
		// S18: ExecutionState::is_active.
		"demoExecutionStateHelpersResult": "true,true,true,false,false,false",
		// S19: PlacementChange::apply.
		"demoPlacementChangeApplyResult": "true|db-b|us-west|false",
		// S20: DecisionEnvelope.
		"demoDecisionEnvelopeResult": "db-b|db-b|none|none|none|none|move|shard 7 (3,4)",
		// S21: MeasurementError displays.
		"demoMeasurementErrorsResult": "UnknownAction|action-99 is not known to this controller|NotAwaitingMeasurement|action-1 is admitted, not awaiting measurement",
		// S22: mechanism no longer registered at advance time.
		"demoMechanismUnregisteredResult": "failed|Fault|mechanism error: mechanism 'stub' is no longer registered|Irreversible|mechanism is no longer registered",
	}

	for field, expected := range want {
		got, _ := d[field].(string)
		if got != expected {
			t.Errorf("%s\n got: %q\nwant: %q", field, got, expected)
		}
	}
}

//! `runtime-cases`: fabric-runtime's own #[test]s (registry.rs, lib.rs,
//! mechanism.rs), replayed step for step, each printed as one
//! `<case>\t<digest>` line. fabric_runtime_cases.fct produces the same
//! digests from the self-hosted port; testdata/fabric_runtime_golden.tsv is
//! this output, and fabric_runtime_test.go compares all three.

use std::cell::RefCell;
use std::rc::Rc;

use fabric_controller::{
    ActionTarget, ExecutionFault, OptimizationAction, PhaseProgress, PlacementExecutor,
    PlacementPlan, PlanPhase, PlanRequest, RollbackOutcome,
};
use fabric_core::{Coordinate, DbmsId, Shard};
use fabric_migration::WriteSeq;
use fabric_protocol::{NodeHeartbeat, NodeRegistration};
use fabric_routing::{ReadPreference, RoutingError};
use fabric_runtime::{CopyVerdict, NodeRegistry, PlacementFabric, PlacementMechanism};
use fabric_topology::TopologyRegistry;

const SHARD: u64 = 7;
const CELL: Coordinate = Coordinate::new(3, 4);

fn target() -> ActionTarget {
    ActionTarget::new(SHARD, CELL)
}

fn node(id: &str, region: &str) -> (NodeRegistration, NodeHeartbeat) {
    (
        NodeRegistration::new(DbmsId::new(id), "0.13.0", region),
        NodeHeartbeat { node_id: DbmsId::new(id), timestamp_ms: 1_000, healthy: true },
    )
}

fn fabric(cells: &[Coordinate]) -> Rc<RefCell<PlacementFabric>> {
    let mut topology = TopologyRegistry::new();
    let shard = Shard::new(SHARD, "social");
    for cell in cells {
        topology.place(DbmsId::new("db-a"), &shard, *cell, "us-east");
    }
    let mut registry = NodeRegistry::new();
    for (id, region) in [("db-a", "us-east"), ("db-b", "us-west"), ("db-c", "us-west")] {
        let (registration, beat) = node(id, region);
        registry.register(&registration, 1_000);
        registry.heartbeat(&beat).expect("registered");
    }
    let mut fabric = PlacementFabric::new();
    fabric.adopt_topology(&topology);
    fabric.observe_nodes(&registry, 1_000, 30_000);
    fabric.record_size(ActionTarget::new(SHARD, cells[0]), 4_096);
    Rc::new(RefCell::new(fabric))
}

fn request(action: OptimizationAction) -> PlanRequest {
    let destination = match &action {
        OptimizationAction::Replicate { target } | OptimizationAction::Move { target } => {
            Some(target.clone())
        }
        _ => None,
    };
    PlanRequest {
        target: target(),
        action,
        source: fabric_topology::Placement::new(
            DbmsId::new("db-a"),
            &Shard::new(SHARD, "social"),
            CELL,
            "us-east",
        ),
        destination,
        replicas: vec![DbmsId::new("db-a")],
        requested_at_ms: 1_000,
    }
}

fn move_to_b() -> PlanRequest {
    request(OptimizationAction::Move { target: DbmsId::new("db-b") })
}

fn err_kind(e: &RoutingError) -> String {
    let d = format!("{e:?}");
    d.split(|c: char| c == ' ' || c == '{' || c == '(').next().unwrap().to_string()
}

fn route(r: Result<fabric_routing::Route, RoutingError>) -> String {
    match r {
        Ok(route) => format!("{}@{:?}", route.node.0, route.served_by),
        Err(e) => format!("E:{}", err_kind(&e)),
    }
}

fn snap(f: &PlacementFabric, t: ActionTarget) -> String {
    let m = match f.migration(t) {
        Some(m) => format!(
            "{}/{}/{}/{}/{}/{}",
            m.phase().label(),
            m.has_cut_over() as u8,
            m.is_write_fenced() as u8,
            m.pending_writes(),
            m.read_owner().0,
            m.write_owner().map(|o| o.0.clone()).unwrap_or("-".into())
        ),
        None => "-".into(),
    };
    let s = match f.replica_set(t) {
        Some(set) => set
            .replicas()
            .map(|r| format!("{}:{}:{}", r.node.0, r.state.label(), if r.is_primary() { "P" } else { "S" }))
            .collect::<Vec<_>>()
            .join(","),
        None => "-".into(),
    };
    let owner = f
        .topology()
        .locate(t.shard_id, t.coordinate)
        .map(|p| p.dbms_id.0.clone())
        .unwrap_or("-".into());
    format!(
        "g={};rg={};m={};s={};o={};r={};w={}",
        f.generation(),
        f.routing().generation(),
        m,
        s,
        owner,
        route(f.resolve_read(t, ReadPreference::Primary)),
        route(f.resolve_write(t))
    )
}

fn progress(p: Result<PhaseProgress, ExecutionFault>) -> String {
    match p {
        Ok(PhaseProgress::Complete) => "complete".into(),
        Ok(PhaseProgress::Running { fraction }) => format!("running({})", (fraction * 1000.0).round() as i64),
        Err(f) => format!("fault({})", f),
    }
}

fn unit(p: Result<(), ExecutionFault>) -> String {
    match p {
        Ok(()) => "ok".into(),
        Err(f) => format!("fault({})", f),
    }
}

fn rollback(p: Result<RollbackOutcome, ExecutionFault>) -> String {
    match p {
        Ok(RollbackOutcome::Restored) => "restored".into(),
        Ok(RollbackOutcome::Irreversible { reason }) => format!("irreversible({reason})"),
        Err(f) => format!("fault({})", f),
    }
}

fn plan_digest(p: &PlacementPlan) -> String {
    format!(
        "adds={};removes={};phases={};bytes={};auth={}",
        p.adds.iter().map(|d| d.0.clone()).collect::<Vec<_>>().join(","),
        p.removes.iter().map(|d| d.0.clone()).collect::<Vec<_>>().join(","),
        p.phases().iter().map(|ph| ph.label()).collect::<Vec<_>>().join(","),
        p.estimated_bytes,
        p.changes_authority() as u8
    )
}

fn mech_cutover_verification() -> String {
    let destination = DbmsId::new("db-b");
    let mut out = Vec::new();
    let drive = |shared: &Rc<RefCell<PlacementFabric>>, mechanism: &mut PlacementMechanism, out: &mut Vec<String>| {
        let plan = mechanism.plan(&move_to_b()).expect("plannable");
        out.push(unit(mechanism.begin_phase(&plan, PlanPhase::Prepare, 1_000)));
        out.push(progress(mechanism.poll_phase(&plan, PlanPhase::Prepare, 1_000)));
        out.push(unit(mechanism.begin_phase(&plan, PlanPhase::Transfer, 2_000)));
        {
            let mut f = shared.borrow_mut();
            f.record_transfer(target(), &destination, 1, 4_096, 3_000);
            f.demand_verification(target(), &destination);
        }
        out.push(progress(mechanism.poll_phase(&plan, PlanPhase::Transfer, 3_000)));
        out.push(unit(mechanism.begin_phase(&plan, PlanPhase::Cutover, 4_000)));
        out.push(snap(&shared.borrow(), target()));
        plan
    };
    let shared = fabric(&[CELL]);
    let mut mechanism = PlacementMechanism::new(Rc::clone(&shared));
    let plan = drive(&shared, &mut mechanism, &mut out);
    out.push(progress(mechanism.poll_phase(&plan, PlanPhase::Cutover, 4_000)));
    out.push(snap(&shared.borrow(), target()));
    shared.borrow_mut().record_verification(target(), &destination, CopyVerdict::Verified { rows: 3, at_ms: 4_500 });
    out.push(progress(mechanism.poll_phase(&plan, PlanPhase::Cutover, 5_000)));
    out.push(snap(&shared.borrow(), target()));

    let shared = fabric(&[CELL]);
    let mut mechanism = PlacementMechanism::new(Rc::clone(&shared));
    let plan = drive(&shared, &mut mechanism, &mut out);
    shared.borrow_mut().record_verification(
        target(),
        &destination,
        CopyVerdict::Failed { reason: "'Post:1' differs in `data`".to_string(), at_ms: 4_500 },
    );
    out.push(progress(mechanism.poll_phase(&plan, PlanPhase::Cutover, 5_000)));
    out.push(snap(&shared.borrow(), target()));
    out.push(rollback(mechanism.abort(&plan, &[PlanPhase::Prepare, PlanPhase::Transfer], 6_000)));
    out.push(snap(&shared.borrow(), target()));
    out.join("|")
}

fn mech_routing_learns() -> String {
    let mut out = Vec::new();
    let shared = fabric(&[CELL]);
    let mut mechanism = PlacementMechanism::new(Rc::clone(&shared));
    let plan = mechanism.plan(&move_to_b()).expect("plannable");
    out.push(plan_digest(&plan));
    out.push(unit(mechanism.begin_phase(&plan, PlanPhase::Prepare, 1_000)));
    out.push(snap(&shared.borrow(), target()));
    out.push(progress(mechanism.poll_phase(&plan, PlanPhase::Prepare, 1_000)));
    out.push(unit(mechanism.begin_phase(&plan, PlanPhase::Transfer, 2_000)));
    out.push(snap(&shared.borrow(), target()));
    out.push(progress(mechanism.poll_phase(&plan, PlanPhase::Transfer, 2_000)));
    let w = shared.borrow_mut().accept_write(target(), WriteSeq(0));
    out.push(format!("write={:?}", w.map_err(|e| e.to_string())));
    shared.borrow_mut().record_transfer(target(), &DbmsId::new("db-b"), 1, 4_096, 3_000);
    out.push(progress(mechanism.poll_phase(&plan, PlanPhase::Transfer, 3_000)));
    out.push(snap(&shared.borrow(), target()));
    out.push(unit(mechanism.begin_phase(&plan, PlanPhase::Cutover, 4_000)));
    out.push(snap(&shared.borrow(), target()));
    out.push(progress(mechanism.poll_phase(&plan, PlanPhase::Cutover, 4_000)));
    let a = shared.borrow_mut().record_applied(target(), WriteSeq(0), 4_500);
    out.push(format!("applied={:?}", a.map_err(|e| e.to_string())));
    out.push(progress(mechanism.poll_phase(&plan, PlanPhase::Cutover, 5_000)));
    out.push(snap(&shared.borrow(), target()));
    out.push(unit(mechanism.begin_phase(&plan, PlanPhase::Cleanup, 6_000)));
    out.push(progress(mechanism.poll_phase(&plan, PlanPhase::Cleanup, 6_000)));
    out.push(snap(&shared.borrow(), target()));
    out.join("|")
}

fn mech_sibling() -> String {
    let mut out = Vec::new();
    let sibling = Coordinate::new(5, 5);
    let st = ActionTarget::new(SHARD, sibling);
    let shared = fabric(&[CELL, sibling]);
    let mut mechanism = PlacementMechanism::new(Rc::clone(&shared));
    let plan = mechanism.plan(&move_to_b()).expect("plannable");
    out.push(plan_digest(&plan));
    out.push(snap(&shared.borrow(), st));
    for (phase, at) in [
        (PlanPhase::Prepare, 1_000),
        (PlanPhase::Transfer, 2_000),
        (PlanPhase::Cutover, 3_000),
        (PlanPhase::Cleanup, 4_000),
    ] {
        out.push(unit(mechanism.begin_phase(&plan, phase, at)));
        out.push(snap(&shared.borrow(), st));
        if phase == PlanPhase::Transfer {
            shared.borrow_mut().record_transfer(target(), &DbmsId::new("db-b"), 1, 4_096, at);
        }
        out.push(progress(mechanism.poll_phase(&plan, phase, at)));
        out.push(snap(&shared.borrow(), target()));
        out.push(snap(&shared.borrow(), st));
    }
    let f = shared.borrow();
    let entry = f.routing().shard(SHARD).unwrap();
    out.push(format!(
        "shardwide={};sibset={};sibmig={};sibling_has_set={}",
        entry.replica_set().is_some() as u8,
        entry.replica_set_at(sibling).is_some() as u8,
        entry.migration_at(sibling).is_some() as u8,
        f.replica_set(st).is_some() as u8
    ));
    out.join("|")
}

fn mech_rollback_before() -> String {
    let mut out = Vec::new();
    let shared = fabric(&[CELL]);
    let mut mechanism = PlacementMechanism::new(Rc::clone(&shared));
    let plan = mechanism.plan(&move_to_b()).unwrap();
    out.push(unit(mechanism.begin_phase(&plan, PlanPhase::Prepare, 1_000)));
    out.push(progress(mechanism.poll_phase(&plan, PlanPhase::Prepare, 1_000)));
    out.push(unit(mechanism.begin_phase(&plan, PlanPhase::Transfer, 2_000)));
    out.push(snap(&shared.borrow(), target()));
    out.push(rollback(mechanism.abort(&plan, &[PlanPhase::Prepare], 3_000)));
    out.push(snap(&shared.borrow(), target()));
    // A second abort finds no job.
    out.push(rollback(mechanism.abort(&plan, &[PlanPhase::Prepare], 3_500)));
    out.join("|")
}

fn mech_rollback_after() -> String {
    let mut out = Vec::new();
    let shared = fabric(&[CELL]);
    let mut mechanism = PlacementMechanism::new(Rc::clone(&shared));
    let plan = mechanism.plan(&move_to_b()).unwrap();
    for (phase, at) in [(PlanPhase::Prepare, 1_000), (PlanPhase::Transfer, 2_000)] {
        out.push(unit(mechanism.begin_phase(&plan, phase, at)));
        if phase == PlanPhase::Transfer {
            shared.borrow_mut().record_transfer(target(), &DbmsId::new("db-b"), 1, 4_096, at);
        }
        out.push(progress(mechanism.poll_phase(&plan, phase, at)));
    }
    out.push(unit(mechanism.begin_phase(&plan, PlanPhase::Cutover, 3_000)));
    out.push(progress(mechanism.poll_phase(&plan, PlanPhase::Cutover, 3_000)));
    out.push(snap(&shared.borrow(), target()));
    out.push(rollback(mechanism.abort(
        &plan,
        &[PlanPhase::Prepare, PlanPhase::Transfer, PlanPhase::Cutover],
        4_000,
    )));
    out.push(snap(&shared.borrow(), target()));
    out.join("|")
}

fn mech_replicate() -> String {
    let mut out = Vec::new();
    let shared = fabric(&[CELL]);
    let mut mechanism = PlacementMechanism::new(Rc::clone(&shared));
    let plan = mechanism
        .plan(&request(OptimizationAction::Replicate { target: DbmsId::new("db-b") }))
        .unwrap();
    out.push(plan_digest(&plan));
    for (phase, at) in [(PlanPhase::Prepare, 1_000), (PlanPhase::Transfer, 2_000)] {
        out.push(unit(mechanism.begin_phase(&plan, phase, at)));
        if phase == PlanPhase::Transfer {
            out.push(progress(mechanism.poll_phase(&plan, phase, at)));
            shared.borrow_mut().record_transfer(target(), &DbmsId::new("db-b"), 1, 4_096, at);
        }
        out.push(progress(mechanism.poll_phase(&plan, phase, at)));
        out.push(snap(&shared.borrow(), target()));
    }
    let f = shared.borrow();
    let entry = f.routing().shard(SHARD).unwrap();
    out.push(format!(
        "fresh={};shardwide={}",
        entry.replica_set_at(CELL).map(|s| s.read_targets(false).len()).unwrap_or(99),
        entry.replica_set().is_some() as u8
    ));
    out.join("|")
}

fn mech_plans() -> String {
    let mut out = Vec::new();
    let shared = fabric(&[CELL]);
    let mechanism = PlacementMechanism::new(shared);
    for action in [
        OptimizationAction::Isolate,
        OptimizationAction::Split,
        OptimizationAction::NoAction,
        OptimizationAction::Move { target: DbmsId::new("db-a") },
        OptimizationAction::Colocate { target: Coordinate::new(1, 1) },
    ] {
        out.push(format!("supports={}", mechanism.supports(&action) as u8));
        match mechanism.plan(&request(action)) {
            Ok(p) => out.push(plan_digest(&p)),
            Err(e) => out.push(format!("err({e})")),
        }
    }
    let mut colocate = request(OptimizationAction::Colocate { target: Coordinate::new(1, 1) });
    colocate.destination = Some(DbmsId::new("db-c"));
    match mechanism.plan(&colocate) {
        Ok(p) => out.push(plan_digest(&p)),
        Err(e) => out.push(format!("err({e})")),
    }
    out.join("|")
}


use fabric_protocol::{FabricMessage, FabricResponse, TelemetryBatch, TelemetrySample};
use fabric_runtime::{FabricRuntime, NodeHealth, DEFAULT_HEARTBEAT_DEADLINE_MS, RECENT_OBSERVATIONS};
use fabric_controller::{ControllerPolicy, DecisionEnvelope, ExecutionState};

fn registration(id: &str) -> NodeRegistration {
    NodeRegistration::new(DbmsId::new(id), "0.13.0", "us-east")
}

fn beat(id: &str, timestamp_ms: u64, healthy: bool) -> NodeHeartbeat {
    NodeHeartbeat { node_id: DbmsId::new(id), timestamp_ms, healthy }
}

fn node_digest(n: &fabric_runtime::RegisteredNode, now: u64, deadline: u64) -> String {
    format!(
        "{}:{}:{}:{}:{}:{}:{}:{}:{}:{}:{}",
        n.id.0,
        n.protocol,
        n.software_version,
        n.region,
        n.registered_at_ms,
        n.last_heartbeat_ms.map(|v| v.to_string()).unwrap_or("-".into()),
        n.reported_healthy as u8,
        n.heartbeats,
        n.health(now, deadline).label(),
        n.silence_ms(now).map(|v| v.to_string()).unwrap_or("-".into()),
        n.health(now, deadline).is_serviceable() as u8
    )
}

fn registry_cases() -> String {
    let mut out = Vec::new();
    let mut r = NodeRegistry::new();
    out.push(format!("reg={}", r.register(&registration("db-a"), 1_000) as u8));
    out.push(format!("len={}", r.len()));
    out.push(node_digest(r.get(&DbmsId::new("db-a")).unwrap(), 1_000, DEFAULT_HEARTBEAT_DEADLINE_MS));
    let mut r = NodeRegistry::new();
    r.register(&registration("db-a"), 0);
    r.heartbeat(&beat("db-a", 10_000, true)).unwrap();
    let n = r.get(&DbmsId::new("db-a")).unwrap();
    for now in [10_000, 40_000, 40_001] {
        out.push(node_digest(n, now, 30_000));
    }
    let mut r = NodeRegistry::new();
    r.register(&registration("db-a"), 0);
    r.heartbeat(&beat("db-a", 10_000, false)).unwrap();
    out.push(node_digest(r.get(&DbmsId::new("db-a")).unwrap(), 10_000, 30_000));
    out.push(format!("degradedServiceable={}", NodeHealth::Degraded.is_serviceable() as u8));
    let mut r = NodeRegistry::new();
    let err = r.heartbeat(&beat("ghost", 1, true)).unwrap_err();
    out.push(format!("ghost={};empty={}", err, r.is_empty() as u8));
    let mut r = NodeRegistry::new();
    r.register(&registration("db-a"), 0);
    r.heartbeat(&beat("db-a", 20_000, true)).unwrap();
    r.heartbeat(&beat("db-a", 5_000, true)).unwrap();
    out.push(node_digest(r.get(&DbmsId::new("db-a")).unwrap(), 20_000, 30_000));
    let mut r = NodeRegistry::new();
    r.register(&registration("db-a"), 0);
    r.heartbeat(&beat("db-a", 10_000, true)).unwrap();
    out.push(format!("rereg={}", r.register(&registration("db-a"), 12_000) as u8));
    out.push(node_digest(r.get(&DbmsId::new("db-a")).unwrap(), 12_000, 30_000));
    let mut r = NodeRegistry::new();
    for id in ["db-c", "db-a", "db-b"] {
        r.register(&registration(id), 0);
    }
    r.heartbeat(&beat("db-a", 10_000, true)).unwrap();
    r.heartbeat(&beat("db-b", 10_000, false)).unwrap();
    let ids: Vec<String> = r.unhealthy(10_000, 30_000).map(|n| n.id.0.clone()).collect();
    out.push(format!("unhealthy={}", ids.join(",")));
    let all: Vec<String> = r.nodes().map(|n| n.id.0.clone()).collect();
    out.push(format!("order={}", all.join(",")));
    out.join("|")
}

fn response(r: &FabricResponse) -> String {
    match r {
        FabricResponse::Acknowledged => "Acknowledged".into(),
        FabricResponse::Registered { node_id } => format!("Registered({node_id})"),
        FabricResponse::Rejected { reason } => format!("Rejected({reason})"),
        other => format!("{other:?}"),
    }
}

fn register(id: &str) -> FabricMessage {
    FabricMessage::RegisterNode(NodeRegistration::new(DbmsId::new(id), "0.13.0", "us-east"))
}

fn heartbeat(id: &str, timestamp_ms: u64, healthy: bool) -> FabricMessage {
    FabricMessage::Heartbeat(beat(id, timestamp_ms, healthy))
}

fn sample(ops: f64, read: f64, write: f64) -> TelemetrySample {
    TelemetrySample {
        coordinate: Coordinate::new(0, 0),
        operations_per_second: ops,
        read_ratio: read,
        write_ratio: write,
        read_latency_us: 0.0,
        write_latency_us: 0.0,
        cpu_utilization: 0.0,
        memory_utilization: 0.0,
        queue_depth: 0,
        cell_breakdown: Vec::new(),
        cell_breakdown_partial: false,
    }
}

fn availability(rt: &FabricRuntime, id: &str) -> String {
    rt.placement()
        .borrow()
        .routing()
        .node(&DbmsId::new(id))
        .map(|n| format!("{:?}", n.availability))
        .unwrap_or("-".into())
}

fn lib_cases() -> String {
    let mut out = Vec::new();
    let mut rt = FabricRuntime::new();
    out.push(response(&rt.handle(register("db-a"))));
    out.push(format!("nodes={}", rt.nodes().len()));

    let mut rt = FabricRuntime::new();
    rt.handle(register("db-a"));
    out.push(response(&rt.handle(heartbeat("db-a", 12_000, true))));
    out.push(format!("clock={}", rt.clock_ms()));
    out.push(node_digest(rt.nodes().get(&DbmsId::new("db-a")).unwrap(), rt.clock_ms(), 30_000));

    let mut rt = FabricRuntime::new();
    out.push(response(&rt.handle(heartbeat("ghost", 12_000, true))));
    out.push(format!("nodes={};clock={}", rt.nodes().len(), rt.clock_ms()));

    let mut rt = FabricRuntime::new();
    rt.handle(register("db-a"));
    rt.handle(register("db-b"));
    rt.handle(heartbeat("db-a", 10_000, true));
    rt.handle(heartbeat("db-b", 10_000, true));
    rt.handle(heartbeat("db-b", 100_000, true));
    let now = rt.clock_ms();
    let stale: Vec<String> = rt.nodes().unhealthy(now, DEFAULT_HEARTBEAT_DEADLINE_MS).map(|n| n.id.0.clone()).collect();
    out.push(format!("clock={};stale={};a={};b={}", now, stale.join(","), availability(&rt, "db-a"), availability(&rt, "db-b")));

    let mut rt = FabricRuntime::new().with_heartbeat_deadline_ms(5_000);
    rt.handle(register("db-a"));
    rt.handle(heartbeat("db-a", 10_000, true));
    out.push(format!("before={}", availability(&rt, "db-a")));
    rt.tick_clock(20_000);
    out.push(format!("clock={};after={}", rt.clock_ms(), availability(&rt, "db-a")));
    rt.tick_clock(1_000);
    out.push(format!("clock={}", rt.clock_ms()));

    let mut rt = FabricRuntime::new();
    for tick in 0..(RECENT_OBSERVATIONS as u64 * 2) {
        rt.handle(FabricMessage::Telemetry(TelemetryBatch {
            timestamp_ms: 1_000 + tick,
            node_id: DbmsId::new("db-a"),
            shard: Shard::new(1, "us-east"),
            samples: vec![sample(1.0, 1.0, 0.0)],
        }));
    }
    out.push(format!(
        "window={};first={};last={};analyzer={};state={}",
        rt.observations().len(),
        rt.observations().first().unwrap().timestamp_ms,
        rt.observations().last().unwrap().timestamp_ms,
        rt.analyzer().len(),
        rt.state().len()
    ));

    let policy = ControllerPolicy { phase_timeout_ms: 1_234, ..ControllerPolicy::default() };
    let rt = FabricRuntime::with_policy(policy);
    out.push(format!("timeout={};mechs={}", rt.controller().policy().phase_timeout_ms, rt.controller().mechanisms().join(",")));

    let mut rt = FabricRuntime::new();
    out.push(response(&rt.handle(FabricMessage::Telemetry(TelemetryBatch {
        timestamp_ms: 7_000,
        node_id: DbmsId::new("db-a"),
        shard: Shard::new(1, "us-east"),
        samples: vec![sample(10.0, 0.9, 0.1)],
    }))));
    let p = rt.profile(&Shard::new(1, "us-east"), Coordinate::new(0, 0)).unwrap();
    out.push(format!(
        "clock={};analyzer={};ops={};r={};w={};pressure={}",
        rt.clock_ms(),
        rt.analyzer().len(),
        (p.operations_per_second * 1000.0).round() as i64,
        (p.read_ratio * 1000.0).round() as i64,
        (p.write_ratio * 1000.0).round() as i64,
        (p.pressure_score * 1_000_000.0).round() as i64
    ));
    out.join("|")
}

fn state_label(s: &Option<ExecutionState>) -> String {
    match s {
        None => "-".into(),
        Some(ExecutionState::Running { phase, .. }) => format!("running:{}", phase.label()),
        Some(other) => other.label().into(),
    }
}

// A loop through the runtime API with no simulator: three nodes, one hot
// cell on db-a, the real optimizer's decision, the controller driving the
// real mechanism through every phase with the fleet reporting in between.
fn runtime_loop() -> String {
    let mut out = Vec::new();
    let mut rt = FabricRuntime::new();
    for (id, region) in [("db-a", "us-east"), ("db-b", "us-west"), ("db-c", "us-west")] {
        rt.handle(FabricMessage::RegisterNode(NodeRegistration::new(DbmsId::new(id), "0.13.0", region)));
    }
    for id in ["db-a", "db-b", "db-c"] {
        rt.handle(heartbeat(id, 1_000, true));
    }
    let mut topology = TopologyRegistry::new();
    let shard = Shard::new(SHARD, "social");
    topology.place(DbmsId::new("db-a"), &shard, CELL, "us-east");
    rt.adopt_topology(topology);
    out.push(snap(&rt.placement().borrow(), target()));
    rt.handle(FabricMessage::Telemetry(TelemetryBatch {
        timestamp_ms: 2_000,
        node_id: DbmsId::new("db-a"),
        shard: shard.clone(),
        samples: vec![TelemetrySample {
            coordinate: CELL,
            operations_per_second: 12_000.0,
            read_ratio: 0.5,
            write_ratio: 0.5,
            read_latency_us: 40_000.0,
            write_latency_us: 60_000.0,
            cpu_utilization: 0.99,
            memory_utilization: 0.95,
            queue_depth: 30_000,
            cell_breakdown: Vec::new(),
            cell_breakdown_partial: false,
        }],
    }));
    let baseline = rt.profile(&shard, CELL).unwrap().clone();
    let decision = rt.optimize(&baseline);
    out.push(format!(
        "decision={};gain={};cost={};conf={}",
        match &decision.action {
            OptimizationAction::Replicate { target } | OptimizationAction::Move { target } => format!("{}:{}", fabric_controller::action_label(&decision.action), target.0),
            OptimizationAction::Colocate { target } => format!("colocate:{},{}", target.x, target.y),
            other => fabric_controller::action_label(other).to_string(),
        },
        (decision.expected_gain * 1e6).round() as i64,
        (decision.estimated_cost * 1e6).round() as i64,
        (decision.confidence * 1e6).round() as i64
    ));
    let view = rt.fleet_view();
    let fleet: Vec<String> = view
        .nodes()
        .map(|n| format!("{}:{}:{}:{}:{}", n.id.0, n.condition.label(), n.hosted_placements, (n.cpu_utilization * 1000.0).round() as i64, (n.memory_utilization * 1000.0).round() as i64))
        .collect();
    out.push(format!("fleet={};gen={}", fleet.join(","), view.generation()));
    let envelope = DecisionEnvelope::new(decision, 2_000, rt.fleet_view().generation());
    let id = match rt.submit(envelope, &baseline) {
        Ok(id) => id,
        Err(e) => {
            out.push(format!("submit=err({e})"));
            return out.join("|");
        }
    };
    let destination = rt.controller().record(id).unwrap().plan().adds[0].clone();
    out.push(format!("submit={};dest={}", id.0, destination.0));
    rt.placement().borrow_mut().record_size(target(), 8_192);
    let mut state: Option<ExecutionState> = Some(ExecutionState::Admitted);
    let mut wrote = false;
    for step in 0..14u64 {
        let at = 3_000 + step * 1_000;
        for n in ["db-a", "db-b", "db-c"] {
            rt.handle(heartbeat(n, at, true));
        }
        if let Some(ExecutionState::Running { phase, .. }) = state {
            if phase == PlanPhase::Transfer {
                let copied = (step * 4_096).min(8_192);
                rt.placement().borrow_mut().record_transfer(target(), &destination, usize::from(copied >= 8_192), copied, at);
                if !wrote {
                    let w = rt.placement().borrow_mut().accept_write(target(), WriteSeq(0));
                    out.push(format!("write={:?}", w.map_err(|e| e.to_string())));
                    wrote = true;
                }
            }
            if phase == PlanPhase::Cutover {
                let a = rt.placement().borrow_mut().record_applied(target(), WriteSeq(0), at);
                out.push(format!("applied={:?}", a.map_err(|e| e.to_string())));
            }
        }
        state = rt.advance(id);
        out.push(format!("{}:{}", at, state_label(&state)));
        out.push(snap(&rt.placement().borrow(), target()));
        out.push(format!(
            "rtowner={}",
            rt.topology().locate(SHARD, CELL).map(|p| p.dbms_id.0.clone()).unwrap_or("-".into())
        ));
        if state == Some(ExecutionState::AwaitingMeasurement) {
            break;
        }
    }
    // Settle, then measure against a cooled cell.
    for n in ["db-a", "db-b", "db-c"] {
        rt.handle(heartbeat(n, 60_000, true));
    }
    let mut after = baseline.clone();
    after.operations_per_second = 3_000.0;
    after.read_latency_us = 5_000.0;
    after.write_latency_us = 8_000.0;
    after.cpu_utilization = 0.40;
    after.memory_utilization = 0.50;
    after.queue_depth = 100;
    match rt.measure(id, &after) {
        Ok(outcome) => out.push(format!("measured={};realized={};ratio={}", outcome.verdict.label(), (outcome.realized_gain * 1e6).round() as i64, (outcome.gain_ratio * 1e6).round() as i64)),
        Err(e) => out.push(format!("measure=err({e})")),
    }
    let reports = rt.controller().reports();
    out.push(format!("reports={};mech={}", reports.len(), reports.first().map(|r| r.mechanism.clone()).unwrap_or_default()));
    out.join("|")
}

// ---------------------------------------------------------------------------
// tests/control_loop.rs: the whole loop against fabric-simulator.
// ---------------------------------------------------------------------------

use fabric_simulator::{ClusterSpec, Fault, Simulation};

fn cl_spec() -> ClusterSpec {
    ClusterSpec {
        seed: 7,
        shards_per_node: 2,
        cells_per_shard: 1,
        classes: vec![fabric_core::WorkloadClass::WriteHeavy],
        ..ClusterSpec::default()
    }
}

fn cl_multi_cell_spec(class: fabric_core::WorkloadClass) -> ClusterSpec {
    ClusterSpec {
        seed: 7,
        shards_per_node: 1,
        cells_per_shard: 2,
        classes: vec![class],
        ..ClusterSpec::default()
    }
}

fn cl_mixed_duress(node: DbmsId, shard: Shard, coordinate: Coordinate, timestamp_ms: u64, read_ratio: f64) -> FabricMessage {
    FabricMessage::Telemetry(TelemetryBatch {
        timestamp_ms,
        node_id: node,
        shard,
        samples: vec![TelemetrySample {
            coordinate,
            operations_per_second: 12_000.0,
            read_ratio,
            write_ratio: 1.0 - read_ratio,
            read_latency_us: 40_000.0,
            write_latency_us: 60_000.0,
            cpu_utilization: 0.99,
            memory_utilization: 0.95,
            queue_depth: 30_000,
            cell_breakdown: Vec::new(),
            cell_breakdown_partial: false,
        }],
    })
}

fn cl_feed(runtime: &mut FabricRuntime, messages: Vec<FabricMessage>) {
    for message in messages {
        runtime.handle(message);
    }
}

fn cl_profile(p: &fabric_workload::WorkloadProfile) -> String {
    format!(
        "ops={};r={};w={};cpu={};mem={};q={};p={}",
        (p.operations_per_second * 1e3).round() as i64,
        (p.read_ratio * 1e6).round() as i64,
        (p.write_ratio * 1e6).round() as i64,
        (p.cpu_utilization * 1e6).round() as i64,
        (p.memory_utilization * 1e6).round() as i64,
        p.queue_depth,
        (p.pressure_score * 1e6).round() as i64
    )
}

fn cl_decision(d: &fabric_optimizer::OptimizationDecision) -> String {
    format!(
        "decision={};gain={};cost={};conf={};exec={}",
        match &d.action {
            OptimizationAction::Replicate { target } | OptimizationAction::Move { target } => format!("{}:{}", fabric_controller::action_label(&d.action), target.0),
            OptimizationAction::Colocate { target } => format!("colocate:{},{}", target.x, target.y),
            other => fabric_controller::action_label(other).to_string(),
        },
        (d.expected_gain * 1e6).round() as i64,
        (d.estimated_cost * 1e6).round() as i64,
        (d.confidence * 1e6).round() as i64,
        d.should_execute() as u8
    )
}

fn cl_outcome(o: &fabric_controller::ExecutionOutcome) -> String {
    format!(
        "verdict={};expected={};realized={};ratio={};latency={};throughput={}",
        o.verdict.label(),
        (o.expected_gain * 1e6).round() as i64,
        (o.realized_gain * 1e6).round() as i64,
        (o.gain_ratio * 1e6).round() as i64,
        (o.latency_delta_us * 1e3).round() as i64,
        (o.throughput_delta * 1e3).round() as i64
    )
}

// execute: the test's own helper, digesting every step. `watch` targets are
// snapshotted before and after each advance.
fn cl_execute(
    runtime: &mut FabricRuntime,
    simulation: &mut Simulation,
    id: fabric_controller::ActionId,
    target: ActionTarget,
    destination: &DbmsId,
    watch: &[ActionTarget],
    out: &mut Vec<String>,
) -> ExecutionState {
    let bytes = simulation.state().borrow().cell(target).unwrap().bytes;
    let rate = simulation.state().borrow().transfer_bytes_per_ms();
    out.push(format!("bytes={bytes};rate={rate}"));
    runtime.placement().borrow_mut().record_size(target, bytes);
    let mut began: Option<u64> = None;
    let mut state = ExecutionState::Admitted;
    let mut mirrored = false;
    for _ in 0..40 {
        let tick = simulation.step();
        cl_feed(runtime, tick.messages);
        let at_ms = simulation.now_ms();
        if let ExecutionState::Running { phase, .. } = state {
            if phase == PlanPhase::Transfer {
                let b = *began.get_or_insert(at_ms);
                let copied = (at_ms.saturating_sub(b) * rate).min(bytes);
                runtime.placement().borrow_mut().record_transfer(target, destination, usize::from(copied >= bytes), copied, at_ms);
            }
        }
        state = runtime.advance(id).expect("the action exists");
        out.push(format!("{}:{}", at_ms, state_label(&Some(state.clone()))));
        for t in watch {
            out.push(snap(&runtime.placement().borrow(), *t));
        }
        let cut_over = runtime.placement().borrow().migration(target).is_some_and(|m| m.has_cut_over());
        if cut_over && !mirrored {
            simulation.state().borrow_mut().set_owner(target, &destination.0);
            mirrored = true;
            out.push("mirrored".into());
        }
        if state == ExecutionState::AwaitingMeasurement {
            if !mirrored {
                simulation.state().borrow_mut().promote(target, &destination.0);
                out.push("promoted".into());
            }
            break;
        }
    }
    state
}

fn cl_settle_and_measure(runtime: &mut FabricRuntime, simulation: &mut Simulation, id: fabric_controller::ActionId, shard: &Shard, coordinate: Coordinate, out: &mut Vec<String>) {
    let mut after = None;
    for _ in 0..45 {
        let tick = simulation.step();
        cl_feed(runtime, tick.messages);
        after = runtime.profile(shard, coordinate).cloned();
    }
    let after = after.expect("the cell keeps reporting");
    out.push(format!("after:{}", cl_profile(&after)));
    match runtime.measure(id, &after) {
        Ok(o) => out.push(cl_outcome(&o)),
        Err(e) => out.push(format!("measure=err({e})")),
    }
    let reports = runtime.controller().reports();
    out.push(format!("reports={};mech={}", reports.len(), reports.first().map(|r| r.mechanism.clone()).unwrap_or_default()));
}

fn cl_owner(rt: &FabricRuntime, t: ActionTarget) -> String {
    rt.topology().locate(t.shard_id, t.coordinate).map(|p| p.dbms_id.0.clone()).unwrap_or("-".into())
}

fn cl_isolate() -> String {
    let mut out = Vec::new();
    let mut simulation = Simulation::new(cl_spec());
    let target = simulation.targets()[0];
    let mut runtime = FabricRuntime::new();
    runtime.adopt_topology(simulation.topology());
    simulation.inject(Fault::HotShard { target, multiplier: 3.0, duration_ms: 900_000 });
    for tick in simulation.run(5) {
        cl_feed(&mut runtime, tick.messages);
    }
    let shard = simulation.state().borrow().shard(target.shard_id).unwrap().clone();
    let source = DbmsId::new(simulation.state().borrow().cell(target).unwrap().owner.clone());
    out.push(format!("target={};source={};clock={}", target, source.0, runtime.clock_ms()));
    runtime.handle(cl_mixed_duress(source.clone(), shard.clone(), target.coordinate, simulation.now_ms(), 0.10));
    let baseline = runtime.profile(&shard, target.coordinate).unwrap().clone();
    out.push(format!("baseline:{}", cl_profile(&baseline)));
    let decision = runtime.optimize(&baseline);
    out.push(cl_decision(&decision));
    let envelope = DecisionEnvelope::new(decision, simulation.now_ms(), runtime.fleet_view().generation());
    let id = match runtime.submit(envelope, &baseline) {
        Ok(id) => id,
        Err(e) => {
            out.push(format!("submit=err({e})"));
            return out.join("|");
        }
    };
    let destination = runtime.controller().record(id).unwrap().plan().adds[0].clone();
    out.push(format!("id={};dest={}", id.0, destination.0));
    out.push(snap(&runtime.placement().borrow(), target));
    // The test's own mid-flight client: one write during the copy, applied
    // during the cutover.
    let bytes = simulation.state().borrow().cell(target).unwrap().bytes;
    let rate = simulation.state().borrow().transfer_bytes_per_ms();
    runtime.placement().borrow_mut().record_size(target, bytes);
    out.push(format!("bytes={bytes};rate={rate}"));
    let mut began: Option<u64> = None;
    let mut writes = 0u64;
    let mut applied = false;
    let mut mirrored = false;
    let mut state = ExecutionState::Admitted;
    for _ in 0..30 {
        let tick = simulation.step();
        cl_feed(&mut runtime, tick.messages);
        let at_ms = simulation.now_ms();
        if let ExecutionState::Running { phase, .. } = state {
            if phase == PlanPhase::Transfer {
                let b = *began.get_or_insert(at_ms);
                let copied = (at_ms.saturating_sub(b) * rate).min(bytes);
                runtime.placement().borrow_mut().record_transfer(target, &destination, usize::from(copied >= bytes), copied, at_ms);
                if writes == 0 {
                    let route = runtime.placement().borrow_mut().accept_write(target, WriteSeq(0));
                    out.push(format!("write={:?}", route.map_err(|e| e.to_string())));
                    writes += 1;
                }
            }
            if phase == PlanPhase::Cutover && !applied {
                out.push(format!("fenced:{}", snap(&runtime.placement().borrow(), target)));
                let a = runtime.placement().borrow_mut().record_applied(target, WriteSeq(0), at_ms);
                out.push(format!("applied={:?}", a.map_err(|e| e.to_string())));
                applied = true;
            }
        }
        state = runtime.advance(id).expect("the action exists");
        out.push(format!("{}:{}", at_ms, state_label(&Some(state.clone()))));
        out.push(snap(&runtime.placement().borrow(), target));
        let cut_over = runtime.placement().borrow().migration(target).is_some_and(|m| m.has_cut_over());
        if cut_over && !mirrored {
            simulation.state().borrow_mut().set_owner(target, &destination.0);
            mirrored = true;
            out.push("mirrored".into());
        }
        if state == ExecutionState::AwaitingMeasurement {
            break;
        }
    }
    {
        let f = runtime.placement().borrow();
        let m = f.migration(target).unwrap();
        out.push(format!("history={};rtowner={}", m.history().len(), cl_owner(&runtime, target)));
    }
    simulation.inject(Fault::HotShard { target, multiplier: 1.0, duration_ms: 0 });
    cl_settle_and_measure(&mut runtime, &mut simulation, id, &shard, target.coordinate, &mut out);
    out.join("|")
}

fn cl_move_one_cell() -> String {
    let mut out = Vec::new();
    let mut simulation = Simulation::new(cl_multi_cell_spec(fabric_core::WorkloadClass::Mixed));
    let targets = simulation.targets();
    let target = targets[0];
    let sibling = *targets.iter().find(|c| c.shard_id == target.shard_id && c.coordinate != target.coordinate).unwrap();
    let mut runtime = FabricRuntime::new();
    runtime.adopt_topology(simulation.topology());
    simulation.inject(Fault::HotShard { target, multiplier: 3.0, duration_ms: 900_000 });
    for tick in simulation.run(5) {
        cl_feed(&mut runtime, tick.messages);
    }
    let shard = simulation.state().borrow().shard(target.shard_id).unwrap().clone();
    let source = DbmsId::new(simulation.state().borrow().cell(target).unwrap().owner.clone());
    out.push(format!("target={};sibling={};source={};sibowner={}", target, sibling, source.0, simulation.state().borrow().cell(sibling).unwrap().owner));
    runtime.handle(cl_mixed_duress(source.clone(), shard.clone(), target.coordinate, simulation.now_ms(), 0.55));
    let baseline = runtime.profile(&shard, target.coordinate).unwrap().clone();
    out.push(format!("baseline:{}", cl_profile(&baseline)));
    let decision = runtime.optimize(&baseline);
    out.push(cl_decision(&decision));
    let destination = match &decision.action {
        OptimizationAction::Move { target } => target.clone(),
        _ => DbmsId::new("-"),
    };
    out.push(format!("known={}", runtime.fleet_view().get(&destination).is_some() as u8));
    let envelope = DecisionEnvelope::new(decision, simulation.now_ms(), runtime.fleet_view().generation());
    let id = match runtime.submit(envelope, &baseline) {
        Ok(id) => id,
        Err(e) => {
            out.push(format!("submit=err({e})"));
            return out.join("|");
        }
    };
    out.push(format!("id={};adds={}", id.0, runtime.controller().record(id).unwrap().plan().adds.iter().map(|d| d.0.clone()).collect::<Vec<_>>().join(",")));
    out.push(snap(&runtime.placement().borrow(), sibling));
    let state = cl_execute(&mut runtime, &mut simulation, id, target, &destination, &[target, sibling], &mut out);
    out.push(format!("final={}", state_label(&Some(state))));
    {
        let f = runtime.placement().borrow();
        let entry = f.routing().shard(target.shard_id).unwrap();
        out.push(format!(
            "sibset={};sibmig={};shardset={};shardmig={};scoped={}",
            f.replica_set(sibling).is_some() as u8,
            f.migration(sibling).is_some() as u8,
            entry.replica_set().is_some() as u8,
            entry.migration().is_some() as u8,
            entry.scoped_cells().iter().map(|c| format!("{},{}", c.x, c.y)).collect::<Vec<_>>().join(";")
        ));
    }
    out.push(format!("rtowner={};sibrtowner={}", cl_owner(&runtime, target), cl_owner(&runtime, sibling)));
    simulation.inject(Fault::HotShard { target, multiplier: 1.0, duration_ms: 0 });
    cl_settle_and_measure(&mut runtime, &mut simulation, id, &shard, target.coordinate, &mut out);
    out.join("|")
}

fn cl_replicate() -> String {
    let mut out = Vec::new();
    let mut simulation = Simulation::new(ClusterSpec { classes: vec![fabric_core::WorkloadClass::ReadHeavy], ..cl_spec() });
    let target = simulation.targets()[0];
    let mut runtime = FabricRuntime::new();
    runtime.adopt_topology(simulation.topology());
    simulation.inject(Fault::HotShard { target, multiplier: 3.0, duration_ms: 900_000 });
    for tick in simulation.run(5) {
        cl_feed(&mut runtime, tick.messages);
    }
    let shard = simulation.state().borrow().shard(target.shard_id).unwrap().clone();
    let source = DbmsId::new(simulation.state().borrow().cell(target).unwrap().owner.clone());
    out.push(format!("target={};source={}", target, source.0));
    runtime.handle(cl_mixed_duress(source.clone(), shard.clone(), target.coordinate, simulation.now_ms(), 0.90));
    let baseline = runtime.profile(&shard, target.coordinate).unwrap().clone();
    out.push(format!("baseline:{}", cl_profile(&baseline)));
    let decision = runtime.optimize(&baseline);
    out.push(cl_decision(&decision));
    let destination = match &decision.action {
        OptimizationAction::Replicate { target } => target.clone(),
        _ => DbmsId::new("-"),
    };
    let view = runtime.fleet_view();
    if let Some(status) = view.get(&destination) {
        out.push(format!("status={}:{}:{}", status.condition.label(), status.is_full() as u8, status.hosted_placements));
    }
    let envelope = DecisionEnvelope::new(decision, simulation.now_ms(), runtime.fleet_view().generation());
    let id = match runtime.submit(envelope, &baseline) {
        Ok(id) => id,
        Err(e) => {
            out.push(format!("submit=err({e})"));
            return out.join("|");
        }
    };
    let state = cl_execute(&mut runtime, &mut simulation, id, target, &destination, &[target], &mut out);
    out.push(format!("final={}", state_label(&Some(state))));
    {
        let f = runtime.placement().borrow();
        let set = f.replica_set(target).unwrap();
        out.push(format!("reads={};rtowner={}", set.read_targets(false).len(), cl_owner(&runtime, target)));
    }
    cl_settle_and_measure(&mut runtime, &mut simulation, id, &shard, target.coordinate, &mut out);
    out.join("|")
}

// Paths the crate's own tests leave untraced: a Workload message, the
// tick-driven advance_actions, an operator abort mid-transfer, and a
// Topology report (shard ids derived from the coordinate).
fn runtime_misc() -> String {
    let mut out = Vec::new();
    let mut rt = FabricRuntime::new();
    for (id, region) in [("db-a", "us-east"), ("db-b", "us-west"), ("db-c", "us-west")] {
        rt.handle(FabricMessage::RegisterNode(NodeRegistration::new(DbmsId::new(id), "0.13.0", region)));
    }
    for id in ["db-a", "db-b", "db-c"] {
        rt.handle(heartbeat(id, 1_000, true));
    }
    let r = rt.handle(FabricMessage::Workload(fabric_protocol::WorkloadObservation {
        node_id: "db-a".into(),
        coordinate: Coordinate::new(1, 2),
        timestamp_ms: 1_500,
        operations_per_second: 800.0,
        read_ratio: 0.25,
        write_ratio: 0.75,
    }));
    let p = rt.profile(&Shard::new(0, "db-a"), Coordinate::new(1, 2)).unwrap();
    out.push(format!(
        "{}|clock={};state={};analyzer={};ops={};r={};w={}",
        response(&r),
        rt.clock_ms(),
        rt.state().len(),
        rt.analyzer().len(),
        (p.operations_per_second * 1000.0).round() as i64,
        (p.read_ratio * 1000.0).round() as i64,
        (p.write_ratio * 1000.0).round() as i64
    ));
    let r2 = rt.handle(FabricMessage::Topology(fabric_protocol::TopologyReport {
        node_id: DbmsId::new("db-a"),
        timestamp_ms: 1_600,
        placements: vec![
            fabric_protocol::Placement { coordinate: Coordinate::new(3, 4), dbms_id: DbmsId::new("db-a"), region: "us-east".into() },
            fabric_protocol::Placement { coordinate: Coordinate::new(0, 1), dbms_id: DbmsId::new("db-c"), region: "us-west".into() },
        ],
    }));
    let mut placed: Vec<String> = rt.topology().placements().map(|p| format!("{}:{},{}:{}", p.shard_id, p.coordinate.x, p.coordinate.y, p.dbms_id.0)).collect();
    placed.sort();
    out.push(format!("{}|clock={};placed={};gen={}", response(&r2), rt.clock_ms(), placed.join(","), rt.placement().borrow().generation()));
    // Two moves, driven by the tick.
    let t1 = ActionTarget::new(51, Coordinate::new(3, 4));
    let t2 = ActionTarget::new(12, Coordinate::new(0, 1));
    let mut ids = Vec::new();
    for (t, dest, src) in [(t1, "db-b", "db-a"), (t2, "db-a", "db-c")] {
        let decision = fabric_optimizer::OptimizationDecision {
            shard_id: t.shard_id,
            coordinate: t.coordinate,
            action: OptimizationAction::Move { target: DbmsId::new(dest) },
            expected_gain: 0.9,
            estimated_cost: 0.1,
            confidence: 0.95,
        };
        let baseline = fabric_workload::WorkloadProfile::from_metrics(t.shard_id, t.coordinate, fabric_telemetry::WorkloadMetrics::default());
        let envelope = DecisionEnvelope::new(decision, 1_600, rt.fleet_view().generation());
        match rt.submit(envelope, &baseline) {
            Ok(id) => {
                out.push(format!("submit={}:{}", id.0, src));
                ids.push(id);
            }
            Err(e) => out.push(format!("submit=err({e})")),
        }
    }
    for step in 0..3u64 {
        let at = 2_000 + step * 1_000;
        for n in ["db-a", "db-b", "db-c"] {
            rt.handle(heartbeat(n, at, true));
        }
        let states = rt.advance_actions();
        out.push(format!(
            "tick{}={}",
            step,
            states.iter().map(|(id, s)| format!("{}:{}", id.0, state_label(&Some(s.clone())))).collect::<Vec<_>>().join(",")
        ));
        out.push(snap(&rt.placement().borrow(), t1));
        out.push(snap(&rt.placement().borrow(), t2));
    }
    // Abort the first mid-transfer; the second keeps running.
    if ids.is_empty() {
        return out.join("|");
    }
    let aborted = rt.abort(ids[0]);
    out.push(format!("abort={}", state_label(&aborted)));
    out.push(snap(&rt.placement().borrow(), t1));
    let again = rt.abort(ids[0]);
    out.push(format!("abortAgain={}", state_label(&again)));
    let unknown = rt.abort(fabric_controller::ActionId(99));
    out.push(format!("abortUnknown={}", state_label(&unknown)));
    let reports = rt.controller().reports();
    out.push(format!("reports={}", reports.iter().map(|r| format!("{}:{}:{}", r.action_id.0, r.verdict.label(), r.failure.clone().unwrap_or_default())).collect::<Vec<_>>().join(";")));
    out.join("|")
}

/// Values past i64::MAX where the Rust types take any u64: heartbeat
/// clocks and liveness, ActionTarget's Ord and Display, ActionId,
/// ValidationError's Display, and the simulator clock.
fn big_u64() -> String {
    const BIG: u64 = 9_223_372_036_854_775_808;
    const MAX: u64 = u64::MAX;
    let mut out = Vec::new();
    let mut r = NodeRegistry::new();
    r.register(&registration("db-a"), BIG);
    r.heartbeat(&beat("db-a", BIG + 10, true)).unwrap();
    r.heartbeat(&beat("db-a", 5, true)).unwrap();
    let n = r.get(&DbmsId::new("db-a")).unwrap();
    for now in [BIG + 20, BIG + 30_011, MAX, 3] {
        out.push(node_digest(n, now, 30_000));
    }
    out.push(node_digest(n, MAX, BIG));
    let mut targets = vec![
        ActionTarget::new(BIG + 1, Coordinate::new(0, 0)),
        ActionTarget::new(1, Coordinate::new(5, 0)),
        ActionTarget::new(MAX, Coordinate::new(0, 0)),
        ActionTarget::new(BIG, Coordinate::new(1, 0)),
        ActionTarget::new(BIG, Coordinate::new(0, 1)),
    ];
    targets.sort();
    out.push(targets.iter().map(|t| t.to_string()).collect::<Vec<_>>().join(";"));
    out.push(fabric_controller::ActionId(MAX).to_string());
    out.push(fabric_controller::ValidationError::StaleDecision { observed_at_ms: BIG, now_ms: MAX, age_ms: MAX - BIG, max_age_ms: BIG + 1 }.to_string());
    out.push(fabric_controller::ValidationError::DecisionFromTheFuture { observed_at_ms: MAX, now_ms: BIG }.to_string());
    out.push(fabric_controller::ValidationError::TopologySuperseded { decision_generation: MAX, current_generation: BIG }.to_string());
    let mut c = fabric_simulator::SimClock::new(BIG, BIG + 7);
    let a = c.advance();
    out.push(format!("{}:{}:{}:{}", c.tick_ms(), a, c.now_ms(), c.ticks()));
    let mut c = fabric_simulator::SimClock::new(5, 0);
    let a = c.advance();
    out.push(format!("{}:{}", c.tick_ms(), a));
    let mut sim = fabric_simulator::Simulation::new(fabric_simulator::ClusterSpec::default());
    sim.schedule_fault(BIG + 1, fabric_simulator::Fault::NodeLoss { node: DbmsId::new("us-east-db-0") });
    sim.schedule_fault(5, fabric_simulator::Fault::NodeLoss { node: DbmsId::new("us-east-db-1") });
    sim.schedule_fault(MAX, fabric_simulator::Fault::PartitionRegion { region: "us-west".into() });
    for _ in 0..2 {
        let tick = sim.step();
        let faults: Vec<String> = tick.faults.iter().map(|f| f.to_string()).collect();
        out.push(format!("{}:{}:{}", tick.at_ms, faults.join(","), tick.observations.len()));
    }
    out.join("|")
}

pub fn print_all() {
    let cases: Vec<(&str, fn() -> String)> = vec![
        ("mechCutoverVerification", mech_cutover_verification),
        ("mechRoutingLearns", mech_routing_learns),
        ("mechSibling", mech_sibling),
        ("mechRollbackBefore", mech_rollback_before),
        ("mechRollbackAfter", mech_rollback_after),
        ("mechReplicate", mech_replicate),
        ("mechPlans", mech_plans),
        ("registry", registry_cases),
        ("lib", lib_cases),
        ("runtimeLoop", runtime_loop),
        ("controlLoopIsolate", cl_isolate),
        ("controlLoopMoveOneCell", cl_move_one_cell),
        ("controlLoopReplicate", cl_replicate),
        ("runtimeMisc", runtime_misc),
        ("bigU64", big_u64),
    ];
    for (name, f) in cases {
        println!("{name}\t{}", f());
    }
}

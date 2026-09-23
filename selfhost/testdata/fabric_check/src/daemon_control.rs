//! `daemon-control`: fabric-daemon's control.rs driven by a script.
//!
//! `ControlPlane` owns sockets and reads the wall clock inside every step,
//! and its steps are private, so a scripted run cannot call it. This is its
//! body, copied step for step (`cycle`, `probe`, `poll`, `decide`,
//! `cooling`, `measure`, `publish_routing`, `drive_movers`,
//! `mover_endpoint`, `record_transfer`, `record_copy`, `abort`, `drain`,
//! `publish_status`), over the real FabricRuntime, the real
//! MoverSupervisor (whose copy tasks are spawned onto a runtime nothing
//! drives, so a started copy is recorded and never runs) and the real
//! Status types — with "now", the sweep's probes and the telemetry sample
//! taken from the script instead of the clock and the network, and the
//! front door reduced to the routing table it was last handed. No
//! placement store (that path is I/O end to end).
//!
//! One script per stdin line; one output line per script: each step's
//! answer, tab-separated. fabric_daemon_cases.fct's dcaControlCase drives
//! the self-hosted port through the same script.

use std::collections::BTreeMap;

use fabric_controller::{ActionId, DecisionEnvelope, ExecutionState, OutcomeReport};
use fabric_core::{Coordinate, DbmsId, Shard};
use fabric_daemon::config::{ConfigFile, MapEnv, Settings};
use fabric_daemon::control::{Abandoned, ControlRequest, ShutdownReport};
use fabric_daemon::liveness::Probe;
use fabric_daemon::mover::MoverSupervisor;
use fabric_daemon::status::{
    ActionStatus, BackendStatus, CopyingStatus, DecisionCounters, KeyspaceStatus,
    MigrationStatus, MoverStatus, PlacementStatus, RefusedCopy, Status, StoreStatus,
};
use fabric_facetql::mover::{CellScope, MoverReport};
use fabric_facetql::FacetqlEndpoint;
use fabric_protocol::{FabricMessage, NodeHeartbeat, NodeRegistration};
use fabric_routing::{NodeAvailability, RoutingKey};
use fabric_runtime::{CopyVerdict, FabricRuntime, NodeHealth, WriteSeq};
use serde_json::Value;

use crate::stdin_lines;

const UNKNOWN_VERSION: &str = "unknown";
const PUBLISHED_HISTORY: usize = 50;

struct Plane {
    settings: Settings,
    runtime: FabricRuntime,
    telemetry: String,
    store_state: String,
    store_error: Option<String>,
    store_writes: u64,
    movers: MoverSupervisor,
    requests: tokio::sync::mpsc::UnboundedSender<ControlRequest>,
    streaming: reqwest::Client,
    accepted_writes: BTreeMap<u64, u64>,
    published_generation: u64,
    door_generation: u64,
    started_at_ms: u64,
    cycles: u64,
    draining: bool,
    next_probe_ms: u64,
    next_poll_ms: u64,
    decisions: DecisionCounters,
    probes: BTreeMap<String, (String, u64)>,
    samples: BTreeMap<String, String>,
    status: Option<Status>,
}

fn u(v: &Value) -> u64 {
    v.as_u64().expect("a u64")
}

fn text(v: &Value) -> String {
    v.as_str().expect("a string").to_string()
}

fn probe_of(v: &Value) -> (DbmsId, Probe) {
    let status = v[2].as_u64().unwrap_or(0) as u16;
    let probe = match v[1].as_str().unwrap() {
        "Serving" => Probe::Serving { status },
        "Answering" => Probe::Answering { status },
        _ => Probe::Unreachable {
            error: text(&v[3]),
        },
    };
    (DbmsId::new(text(&v[0])), probe)
}

fn verdict_of(v: &Value) -> CopyVerdict {
    match v["kind"].as_str().unwrap() {
        "Verified" => CopyVerdict::Verified {
            rows: u(&v["rows"]),
            at_ms: u(&v["at_ms"]),
        },
        "Failed" => CopyVerdict::Failed {
            reason: text(&v["reason"]),
            at_ms: u(&v["at_ms"]),
        },
        _ => CopyVerdict::Pending,
    }
}

fn report_of(v: &Value) -> MoverReport {
    MoverReport {
        rows_copied: u(&v["rows"]),
        bytes_copied: u(&v["bytes"]),
        resident_bytes: v["resident"].as_u64(),
        snapshot_complete: v["snapshot_complete"].as_bool().unwrap(),
        observed_writes: u(&v["observed"]),
        applied_writes: u(&v["applied"]),
        verdict: verdict_of(&v["verdict"]),
    }
}

impl Plane {
    /// ControlPlane::boot, up to its first cycle, with the declared map.
    fn boot(settings: Settings, telemetry: String, started_at_ms: u64) -> Self {
        let mut runtime = FabricRuntime::with_policy(settings.policy)
            .with_placement_capacity(settings.placement_capacity)
            .with_heartbeat_deadline_ms(settings.silence_budget_ms);

        runtime.tick_clock(started_at_ms);

        for backend in &settings.backends {
            runtime.handle(FabricMessage::RegisterNode(NodeRegistration::new(
                backend.id.clone(),
                UNKNOWN_VERSION,
                backend.region.clone(),
            )));
        }

        runtime.adopt_topology(settings.topology.clone());

        let door_generation = runtime.placement().borrow().routing().generation();
        let (requests, _receiver) = tokio::sync::mpsc::unbounded_channel();

        Self {
            settings,
            runtime,
            telemetry,
            store_state: "not configured: the placement map is declared, not durable".to_string(),
            store_error: None,
            store_writes: 0,
            movers: MoverSupervisor::new(),
            requests,
            streaming: reqwest::Client::builder().build().unwrap(),
            accepted_writes: BTreeMap::new(),
            published_generation: 0,
            door_generation,
            started_at_ms,
            cycles: 0,
            draining: false,
            next_probe_ms: 0,
            next_poll_ms: 0,
            decisions: DecisionCounters::default(),
            probes: BTreeMap::new(),
            samples: BTreeMap::new(),
            status: None,
        }
    }

    fn cycle(&mut self, now: u64, step: &Value) {
        self.runtime.tick_clock(now);

        if now >= self.next_probe_ms {
            let swept: Vec<(DbmsId, Probe)> = step["probes"]
                .as_array()
                .map(|a| a.iter().map(probe_of).collect())
                .unwrap_or_default();
            self.probe(now, swept);
            self.next_probe_ms = now + self.settings.cadence.liveness_probe_ms;
        }

        if now >= self.next_poll_ms {
            self.poll(&step["sample"]);
            self.next_poll_ms = now + self.settings.cadence.telemetry_poll_ms;
        }

        if !self.draining {
            self.decide(now);
        }

        self.runtime.advance_actions();
        self.drive_movers();
        self.measure();
        self.publish_routing();
        self.cycles += 1;
        self.publish_status(now);
    }

    fn probe(&mut self, now_ms: u64, swept: Vec<(DbmsId, Probe)>) {
        for (id, probe) in swept {
            self.probes.insert(id.0.clone(), (probe.describe(), now_ms));

            if !probe.reached() {
                continue;
            }

            self.runtime.handle(FabricMessage::Heartbeat(NodeHeartbeat {
                node_id: id,
                timestamp_ms: now_ms,
                healthy: probe.healthy(),
            }));
        }
    }

    fn poll(&mut self, sample: &Value) {
        if let Some(notes) = sample["notes"].as_array() {
            for note in notes {
                self.samples.insert(text(&note[0]), text(&note[1]));
            }
        }

        if let Some(messages) = sample["messages"].as_array() {
            for message in messages {
                let message: FabricMessage =
                    serde_json::from_str(message.as_str().unwrap()).expect("a FabricMessage");
                self.runtime.handle(message);
            }
        }
    }

    fn decide(&mut self, now: u64) {
        let generation = self.runtime.fleet_view().generation();

        let hot: Vec<_> = self
            .runtime
            .analyzer()
            .profiles()
            .filter(|profile| profile.is_hot())
            .cloned()
            .collect();

        for profile in hot {
            let target =
                fabric_controller::ActionTarget::new(profile.shard_id, profile.coordinate);

            if self.cooling(target, now) {
                continue;
            }

            let decision = self.runtime.optimize(&profile);

            if !decision.should_execute() {
                continue;
            }

            let Some(observed_at_ms) = self.observed_at_ms(profile.shard_id, profile.coordinate)
            else {
                continue;
            };

            self.decisions.proposed += 1;

            let envelope = DecisionEnvelope::new(decision, observed_at_ms, generation);

            match self.runtime.submit(envelope, &profile) {
                Ok(_) => self.decisions.admitted += 1,

                Err(error) => {
                    self.decisions.refused += 1;
                    self.decisions.last_refusal = Some(error.to_string());
                }
            }
        }
    }

    fn cooling(&self, target: fabric_controller::ActionTarget, now_ms: u64) -> bool {
        self.runtime
            .controller()
            .actions()
            .records()
            .filter(|record| record.target() == target && record.state().is_terminal())
            .map(|record| record.report().concluded_at_ms)
            .max()
            .is_some_and(|concluded_at_ms| {
                now_ms.saturating_sub(concluded_at_ms) < self.settings.decision_cooldown_ms
            })
    }

    fn measure(&mut self) {
        let waiting: Vec<(ActionId, u64, Coordinate)> = self
            .runtime
            .controller()
            .awaiting_measurement()
            .map(|record| {
                let target = record.target();
                (record.id(), target.shard_id, target.coordinate)
            })
            .collect();

        for (id, shard_id, coordinate) in waiting {
            let Some(after) = self
                .runtime
                .analyzer()
                .profile(&Shard::new(shard_id, ""), coordinate)
                .cloned()
            else {
                continue;
            };

            let _ = self.runtime.measure(id, &after);
        }
    }

    fn publish_routing(&mut self) {
        let generation = {
            let fabric = self.runtime.placement().borrow();

            if fabric.routing().generation() == self.published_generation {
                return;
            }

            fabric.routing().generation()
        };

        self.published_generation = generation;
        self.door_generation = generation;
    }

    fn drive_movers(&mut self) {
        let relocating: Vec<(ActionId, DbmsId, DbmsId, Coordinate, u64)> = self
            .runtime
            .controller()
            .in_flight()
            .filter(|record| !record.plan().removes.is_empty())
            .filter(|record| !record.has_cut_over())
            .filter_map(|record| {
                let target = record.target();

                destination_of(record).map(|destination| {
                    (
                        record.id(),
                        record.source().clone(),
                        destination,
                        target.coordinate,
                        target.shard_id,
                    )
                })
            })
            .collect();

        for (id, source, destination, coordinate, shard_id) in relocating {
            if self.draining {
                continue;
            }

            let scope = match RoutingKey::new(shard_id, coordinate) {
                Ok(key) => CellScope::for_key(&self.settings.keyspace, key),

                Err(error) => {
                    self.movers.refuse(id, error.to_string());
                    continue;
                }
            };

            let endpoints = (
                self.mover_endpoint(&source),
                self.mover_endpoint(&destination),
            );

            match endpoints {
                (Ok(source), Ok(destination)) => {
                    self.movers.ensure(
                        id,
                        &source,
                        &destination,
                        scope,
                        self.streaming.clone(),
                        self.requests.clone(),
                    );
                }

                (Err(reason), _) | (_, Err(reason)) => {
                    self.movers.refuse(id, reason);
                }
            }
        }

        let live: std::collections::BTreeSet<u64> = self
            .runtime
            .controller()
            .in_flight()
            .filter(|record| !record.has_cut_over())
            .map(|record| record.id().0)
            .collect();

        self.movers.retain(|id| live.contains(&id.0));
        self.accepted_writes.retain(|id, _| live.contains(id));
    }

    fn mover_endpoint(&self, node: &DbmsId) -> Result<FacetqlEndpoint, String> {
        let backend = self
            .settings
            .backend(node)
            .ok_or_else(|| format!("'{}' is not a declared backend", node.0))?;

        let token = backend.token.clone().ok_or_else(|| {
            format!(
                "'{}' has no configured credential, so this daemon cannot copy \
                 to or from it; declare `token_env` for it, or move the data \
                 with an out-of-band mover reporting through \
                 POST /actions/{{id}}/transfer",
                node.0
            )
        })?;

        FacetqlEndpoint::new(node.clone(), backend.url.clone(), token)
            .map_err(|error| error.to_string())
    }

    fn record_transfer(
        &mut self,
        id: ActionId,
        atoms_copied: usize,
        bytes_copied: u64,
        resident_bytes: Option<u64>,
        at_ms: u64,
    ) -> Result<String, String> {
        let record = self
            .runtime
            .controller()
            .record(id)
            .ok_or_else(|| format!("no action {id}"))?;

        if record.state().is_terminal() {
            return Err(format!(
                "{id} has already concluded ({})",
                record.state().label()
            ));
        }

        let target = record.target();

        let destination = destination_of(record)
            .ok_or_else(|| format!("{id} copies nothing: its plan adds no copy"))?;

        {
            let placement = self.runtime.placement();
            let mut fabric = placement.borrow_mut();

            if let Some(bytes) = resident_bytes {
                fabric.record_size(target, bytes);
            }

            fabric.record_transfer(target, &destination, atoms_copied, bytes_copied, at_ms);
        }

        Ok(format!(
            "{id}: {bytes_copied} byte(s), {atoms_copied} atom(s) recorded against '{}'",
            destination.0
        ))
    }

    fn record_copy(
        &mut self,
        id: ActionId,
        report: &MoverReport,
        at_ms: u64,
    ) -> Result<String, String> {
        let record = self
            .runtime
            .controller()
            .record(id)
            .ok_or_else(|| format!("no action {id}"))?;

        if record.state().is_terminal() {
            return Err(format!(
                "{id} has already concluded ({})",
                record.state().label()
            ));
        }

        let target = record.target();

        let destination = destination_of(record)
            .ok_or_else(|| format!("{id} copies nothing: its plan adds no copy"))?;

        let atoms = usize::from(report.snapshot_complete && report.verdict.is_verified());

        let mut accepted = self.accepted_writes.get(&id.0).copied().unwrap_or(0);

        {
            let placement = self.runtime.placement();
            let mut fabric = placement.borrow_mut();

            fabric.demand_verification(target, &destination);

            if let Some(bytes) = report.resident_bytes {
                fabric.record_size(target, bytes);
            }

            fabric.record_transfer(target, &destination, atoms, report.bytes_copied, at_ms);

            while accepted < report.observed_writes {
                if fabric.accept_write(target, WriteSeq(accepted)).is_err() {
                    break;
                }

                accepted += 1;
            }

            let applied = report.applied_writes.min(accepted);

            if applied > 0 {
                let _ = fabric.record_applied(target, WriteSeq(applied - 1), at_ms);
            }

            fabric.record_verification(target, &destination, report.verdict.clone());
        }

        self.accepted_writes.insert(id.0, accepted);

        Ok(format!(
            "{id}: {} row(s), {} byte(s) onto '{}'; {} change(s) seen, {} applied; copy {}",
            report.rows_copied,
            report.bytes_copied,
            destination.0,
            report.observed_writes,
            report.applied_writes,
            report.verdict.label()
        ))
    }

    fn abort(&mut self, id: ActionId) -> Result<String, String> {
        let record = self
            .runtime
            .controller()
            .record(id)
            .ok_or_else(|| format!("no action {id}"))?;

        if record.state().is_terminal() {
            return Err(format!(
                "{id} has already concluded ({})",
                record.state().label()
            ));
        }

        if record.has_cut_over() {
            return Err(format!(
                "{id} has already cut over: authority for {} is on '{}', so \
                 aborting cannot restore the previous arrangement. Moving it \
                 back is a new decision in the other direction.",
                record.target(),
                destination_of(record)
                    .map(|node| node.0)
                    .unwrap_or_else(|| "the destination".to_string())
            ));
        }

        let state = self
            .runtime
            .abort(id)
            .ok_or_else(|| format!("no action {id}"))?;

        Ok(format!("{id}: {}", state.label()))
    }

    /// `drain`, with the waiting loop's cycles taken from the script: the
    /// loop stops when nothing is admitted or running, when a scripted
    /// cycle's instant reaches the deadline, or when the script runs out.
    fn drain(&mut self, now: u64, cycles: &[Value]) -> ShutdownReport {
        self.draining = true;
        self.movers.stop_all();

        let deadline = now + self.settings.drain_ms;
        let mut report = ShutdownReport::default();

        let in_flight: Vec<(ActionId, bool)> = self
            .runtime
            .controller()
            .in_flight()
            .map(|record| (record.id(), record.has_cut_over()))
            .collect();

        for (id, cut_over) in in_flight {
            if cut_over {
                continue;
            }

            let Some(state) = self.runtime.abort(id) else {
                continue;
            };

            if matches!(state, ExecutionState::RolledBack) {
                report.rolled_back.push(id.0);
                continue;
            }

            if let Some(record) = self.runtime.controller().record(id) {
                report.abandoned.push(abandoned(record));
            }
        }

        self.publish_routing();

        let mut last = now;

        for step in cycles {
            let unfinished = self
                .runtime
                .controller()
                .in_flight()
                .any(|record| {
                    matches!(
                        record.state(),
                        ExecutionState::Admitted | ExecutionState::Running { .. }
                    )
                });

            let at = u(&step["now"]);

            if !unfinished || at >= deadline {
                break;
            }

            self.cycle(at, step);
            last = at;
        }

        for record in self.runtime.controller().actions().records() {
            match record.state() {
                ExecutionState::AwaitingMeasurement => {
                    report.unmeasured.push(record.id().0);
                }

                ExecutionState::Admitted | ExecutionState::Running { .. } => {
                    report.abandoned.push(abandoned(record));
                }

                _ => {}
            }
        }

        let phases: BTreeMap<u64, String> = {
            let fabric = self.runtime.placement().borrow();

            self.runtime
                .controller()
                .actions()
                .records()
                .filter_map(|record| {
                    fabric
                        .migration(record.target())
                        .map(|migration| (record.id().0, migration.phase().label().to_string()))
                })
                .collect()
        };

        for abandoned in &mut report.abandoned {
            abandoned.migration_phase = phases.get(&abandoned.id).cloned();
        }

        report.unpersisted = self.store_error.clone();

        self.publish_routing();
        self.publish_status(last);

        report
    }

    fn observed_at_ms(&self, shard_id: u64, coordinate: Coordinate) -> Option<u64> {
        self.runtime
            .state()
            .observations()
            .filter(|observation| {
                observation.shard.id == shard_id && observation.coordinate == coordinate
            })
            .map(|observation| observation.timestamp_ms)
            .max()
    }

    fn publish_status(&mut self, at_ms: u64) {
        let fleet = self.runtime.fleet_view();
        let clock_ms = self.runtime.clock_ms();

        let backends = self
            .settings
            .backends
            .iter()
            .map(|backend| {
                let node = self.runtime.nodes().get(&backend.id);

                let health = node
                    .map(|node| node.health(clock_ms, self.settings.silence_budget_ms))
                    .unwrap_or(NodeHealth::Unreachable);

                let availability = self
                    .runtime
                    .placement()
                    .borrow()
                    .routing()
                    .node(&backend.id)
                    .map(|entry| availability_label(entry.availability))
                    .unwrap_or("unknown");

                let (last_probe, last_probe_at_ms) = self
                    .probes
                    .get(&backend.id.0)
                    .map(|(verdict, at)| (verdict.clone(), Some(*at)))
                    .unwrap_or_else(|| ("not probed yet".to_string(), None));

                BackendStatus {
                    id: backend.id.0.clone(),
                    url: backend.url.clone(),
                    region: backend.region.clone(),
                    availability: availability.to_string(),
                    health: health.label().to_string(),
                    last_probe,
                    last_probe_at_ms,
                    silence_ms: node.and_then(|node| node.silence_ms(clock_ms)),
                    heartbeats: node.map(|node| node.heartbeats).unwrap_or(0),
                    telemetry: backend.token.is_some(),
                    last_sample: self.samples.get(&backend.id.0).cloned(),
                }
            })
            .collect();

        let placements = {
            let fabric = self.runtime.placement().borrow();

            let mut placements: Vec<PlacementStatus> = self
                .runtime
                .topology()
                .placements()
                .map(|placement| {
                    let target = fabric_controller::ActionTarget::new(
                        placement.shard_id,
                        placement.coordinate,
                    );

                    PlacementStatus {
                        shard: placement.shard_id,
                        x: placement.coordinate.x,
                        y: placement.coordinate.y,
                        holder: placement.dbms_id.0.clone(),
                        region: placement.region.clone(),
                        migration: fabric.migration(target).map(|migration| MigrationStatus {
                            phase: migration.phase().label().to_string(),
                            source: migration.plan().source.0.clone(),
                            destination: migration.plan().destination.0.clone(),
                            read_owner: migration.read_owner().0.clone(),
                            write_fenced: migration.is_write_fenced(),
                            has_cut_over: migration.has_cut_over(),
                        }),
                    }
                })
                .collect();

            placements.sort_by_key(|placement| (placement.shard, placement.y, placement.x));

            placements
        };

        let in_flight = {
            let fabric = self.runtime.placement().borrow();

            self.runtime
                .controller()
                .in_flight()
                .map(|record| {
                    let target = record.target();

                    let (phase, fraction) = match record.state() {
                        ExecutionState::Running { phase, fraction } => {
                            (Some(phase.label().to_string()), Some(*fraction))
                        }
                        _ => (None, None),
                    };

                    let destination = destination_of(record);

                    let transfer = destination
                        .as_ref()
                        .and_then(|node| fabric.transfer(target, node));

                    ActionStatus {
                        id: record.id().0,
                        shard: target.shard_id,
                        x: target.coordinate.x,
                        y: target.coordinate.y,
                        action: record.envelope().action_label().to_string(),
                        mechanism: record.mechanism().to_string(),
                        state: record.state().label().to_string(),
                        phase,
                        fraction,
                        source: record.source().0.clone(),
                        destination: destination_of(record).map(|node| node.0),
                        admitted_at_ms: record.admitted_at_ms(),
                        has_cut_over: record.has_cut_over(),
                        bytes_copied: transfer.map(|progress| progress.bytes_copied).unwrap_or(0),
                        resident_bytes: fabric.size(target),
                    }
                })
                .collect()
        };

        let mut history: Vec<OutcomeReport> = self.runtime.controller().reports();

        if history.len() > PUBLISHED_HISTORY {
            history.drain(..history.len() - PUBLISHED_HISTORY);
        }

        let keyspace = KeyspaceStatus {
            rules: self
                .settings
                .keyspace
                .rules()
                .iter()
                .map(|rule| format!("{} / {}* -> {}", rule.kind(), rule.address_prefix(), rule.key()))
                .collect(),
            fallback: self.settings.keyspace.fallback().map(|key| key.to_string()),
            spans_one_place: self.settings.keyspace.spanning_key().is_some(),
        };

        self.status = Some(Status {
            version: "0.1.0",
            started_at_ms: self.started_at_ms,
            snapshot_at_ms: at_ms,
            clock_ms,
            cycles: self.cycles,
            draining: self.draining,
            data_listen: self.settings.data_listen.to_string(),
            admin_listen: self.settings.admin_listen.to_string(),
            routing_generation: self.door_generation,
            placement_generation: fleet.generation(),
            telemetry_source: self.telemetry.clone(),
            observations: self.runtime.state().len(),
            profiles: self.runtime.analyzer().len(),
            hot_cells: self.runtime.analyzer().hot_coordinates().len(),
            keyspace,
            backends,
            placements,
            in_flight,
            decisions: self.decisions.clone(),
            movers: MoverStatus {
                copying: self
                    .movers
                    .active()
                    .into_iter()
                    .map(|(id, destination)| CopyingStatus { id, destination })
                    .collect(),
                refused: self
                    .movers
                    .refusals()
                    .into_iter()
                    .map(|refusal| RefusedCopy {
                        id: refusal.id,
                        reason: refusal.reason,
                    })
                    .collect(),
            },
            placement_store: StoreStatus {
                configured: self.settings.placement_store.as_ref().map(|id| id.0.clone()),
                state: self.store_state.clone(),
                last_error: self.store_error.clone(),
                writes: self.store_writes,
            },
            history,
        });
    }

    fn status_json(&self) -> String {
        serde_json::to_string(self.status.as_ref().expect("a published status")).unwrap()
    }
}

fn abandoned(record: &fabric_controller::ExecutionRecord) -> Abandoned {
    Abandoned {
        id: record.id().0,
        target: record.target().to_string(),
        source: record.source().0.clone(),
        destination: destination_of(record).map(|node| node.0),
        state: record.state().label().to_string(),
        migration_phase: None,
    }
}

fn destination_of(record: &fabric_controller::ExecutionRecord) -> Option<DbmsId> {
    record
        .plan()
        .adds
        .first()
        .cloned()
        .or_else(|| record.destination().cloned())
}

fn availability_label(availability: NodeAvailability) -> &'static str {
    match availability {
        NodeAvailability::Serviceable => "serviceable",
        NodeAvailability::Unreachable => "unreachable",
        NodeAvailability::Unknown => "unknown",
    }
}

/// main.rs's lines for a shutdown report, `|`-joined.
fn report_lines(report: &ShutdownReport) -> String {
    let mut out = Vec::new();

    for id in &report.rolled_back {
        out.push(format!("fabricd: action-{id} was rolled back; the arrangement is restored"));
    }

    for id in &report.unmeasured {
        out.push(format!(
            "fabricd: action-{id} executed and was never measured; its verdict is lost, \
             its data is not"
        ));
    }

    if let Some(unpersisted) = &report.unpersisted {
        out.push(format!("fabricd: {unpersisted}"));
    }

    for abandoned in &report.abandoned {
        out.push(format!(
            "fabricd: action-{} is past its cutover and did not finish: {} moved from '{}' \
             to '{}', migration phase '{}', execution state '{}'. It was NOT aborted -- an \
             abort after cutover cannot restore the previous arrangement, and recording one \
             would claim a rollback that did not happen. The data is on the destination.",
            abandoned.id,
            abandoned.target,
            abandoned.source,
            abandoned.destination.as_deref().unwrap_or("(none)"),
            abandoned.migration_phase.as_deref().unwrap_or("(none)"),
            abandoned.state,
        ));
    }

    if report.is_clean() {
        out.push("fabricd: stopped cleanly".to_string());
    }

    let code = if report.is_clean() { 0 } else { 75 };
    format!("exit={code}|{}", out.join("|"))
}

fn answer(result: Result<String, String>) -> String {
    match result {
        Ok(message) => format!("ok:{message}"),
        Err(reason) => format!("err:{reason}"),
    }
}

/// One script: `{"config", "env", "telemetry", "steps": [...]}`.
fn control_case(line: &str) -> String {
    let script: Value = serde_json::from_str(line).expect("script json");
    let pairs: Vec<(String, String)> = script["env"]
        .as_array()
        .unwrap()
        .iter()
        .map(|p| (text(&p[0]), text(&p[1])))
        .collect();
    let refs: Vec<(&str, &str)> = pairs.iter().map(|(a, b)| (a.as_str(), b.as_str())).collect();
    let file: ConfigFile = serde_json::from_str(script["config"].as_str().unwrap()).expect("config");
    let settings = Settings::resolve(file, &MapEnv::of(&refs)).expect("settings");

    // Copy tasks are spawned here and never driven.
    let runtime = tokio::runtime::Builder::new_current_thread().build().unwrap();
    let _entered = runtime.enter();

    let mut plane: Option<Plane> = None;
    let mut out = Vec::new();

    for step in script["steps"].as_array().unwrap() {
        let now = step["now"].as_u64().unwrap_or(0);

        match step["op"].as_str().unwrap() {
            "boot" => {
                plane = Some(Plane::boot(settings.clone(), text(&script["telemetry"]), now));
                out.push("boot".to_string());
            }

            "cycle" => {
                let p = plane.as_mut().unwrap();
                p.cycle(now, step);
                out.push(p.status_json());
            }

            "transfer" => {
                let p = plane.as_mut().unwrap();
                let id = ActionId(u(&step["id"]));
                let result = p.record_transfer(
                    id,
                    (u(&step["atoms"]) as usize).min(1),
                    u(&step["bytes"]),
                    step["resident"].as_u64(),
                    now,
                );
                if result.is_ok() {
                    p.runtime.advance(id);
                    p.publish_routing();
                    p.publish_status(now);
                }
                out.push(format!("{}|{}", answer(result), p.status_json()));
            }

            "copy" => {
                let p = plane.as_mut().unwrap();
                let id = ActionId(u(&step["id"]));
                let result = p.record_copy(id, &report_of(&step["report"]), now);
                if result.is_ok() {
                    p.runtime.advance(id);
                    p.publish_routing();
                    p.publish_status(now);
                }
                out.push(format!("{}|{}", answer(result), p.status_json()));
            }

            "abort" => {
                let p = plane.as_mut().unwrap();
                let result = p.abort(ActionId(u(&step["id"])));
                p.publish_routing();
                p.publish_status(now);
                out.push(format!("{}|{}", answer(result), p.status_json()));
            }

            "drain" => {
                let p = plane.as_mut().unwrap();
                let cycles = step["cycles"].as_array().cloned().unwrap_or_default();
                let report = p.drain(now, &cycles);
                out.push(format!("{}|{}", report_lines(&report), p.status_json()));
            }

            other => panic!("unknown step {other:?}"),
        }
    }

    out.join("\t")
}

pub fn run(mode: &str) {
    match mode {
        "daemon-control" => {
            for line in stdin_lines() {
                println!("{}", control_case(&line));
            }
        }
        other => {
            eprintln!("fabric_check: unknown daemon mode {other:?}");
            std::process::exit(2);
        }
    }
}

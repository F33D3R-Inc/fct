//! `daemon-*`: fabric-daemon (fabricd) — config.rs's ConfigFile/Settings,
//! status.rs's JSON, liveness.rs, control.rs and admin.rs — for the
//! self-hosted port in selfhost/fabric_daemon_*.fct. fabric_daemon_test.go
//! feeds both sides the same lines and compares.

use fabric_daemon::config::{ConfigFile, MapEnv, Settings};
use fabric_daemon::status::{
    ActionStatus, BackendStatus, CopyingStatus, DecisionCounters, KeyspaceStatus,
    MigrationStatus, MoverStatus, PlacementStatus, RefusedCopy, Status, StoreStatus,
};
use fabric_daemon::liveness::{LivenessProber, Probe, ProbeTarget};
use fabric_routing::ReadPreference;
use serde_json::Value;
use std::time::Duration;

use crate::stdin_lines;

fn u64_json(v: f64) -> String {
    serde_json::to_string(&v).unwrap()
}

/// One resolved Settings as a single line; see fabric_daemon_cases.fct's
/// dcaSettingsDigest for the same text built from the port.
pub fn settings_digest(s: &Settings) -> String {
    let mut out = Vec::new();
    out.push(format!("data={}", s.data_listen));
    out.push(format!("admin={}", s.admin_listen));
    out.push(format!("token={}", s.admin_token));
    let backends: Vec<String> = s
        .backends
        .iter()
        .map(|b| {
            let cells: Vec<String> = b
                .placements
                .iter()
                .map(|c| format!("{}/{},{}", c.shard, c.x, c.y))
                .collect();
            format!(
                "{}|{}|{}|{}|{}",
                b.id.0,
                b.url,
                b.region,
                b.token.as_deref().unwrap_or("~"),
                cells.join(" ")
            )
        })
        .collect();
    out.push(format!("backends=[{}]", backends.join(";")));
    let rules: Vec<String> = s
        .keyspace
        .rules()
        .iter()
        .map(|r| format!("{} / {}* -> {}", r.kind(), r.address_prefix(), r.key()))
        .collect();
    out.push(format!("rules=[{}]", rules.join(";")));
    out.push(format!(
        "fallback={}",
        s.keyspace.fallback().map(|k| k.to_string()).unwrap_or("-".into())
    ));
    out.push(format!(
        "span={}",
        s.keyspace.spanning_key().map(|k| k.to_string()).unwrap_or("-".into())
    ));
    let mut placements: Vec<(u64, u8, u8, String, String)> = s
        .topology
        .placements()
        .map(|p| {
            (
                p.shard_id,
                p.coordinate.y,
                p.coordinate.x,
                p.dbms_id.0.clone(),
                p.region.clone(),
            )
        })
        .collect();
    placements.sort();
    let placements: Vec<String> = placements
        .iter()
        .map(|(shard, y, x, id, region)| format!("{shard}/{x},{y}@{id}/{region}"))
        .collect();
    out.push(format!("topology=[{}]", placements.join(" ")));
    let pref = match &s.front_door.read_preference {
        ReadPreference::Primary => "Primary".to_string(),
        ReadPreference::AnyFresh => "AnyFresh".to_string(),
        ReadPreference::PreferRegion { region } => format!("PreferRegion:{region}"),
        ReadPreference::AnyCopy => "AnyCopy".to_string(),
    };
    out.push(format!("read={pref}"));
    out.push(format!(
        "cadence={},{},{}",
        s.cadence.liveness_probe_ms, s.cadence.telemetry_poll_ms, s.cadence.control_cycle_ms
    ));
    let p = &s.policy;
    out.push(format!(
        "policy={},{},{},{},{},{},{},{},{},{}",
        p.max_decision_age_ms,
        u64_json(p.min_confidence),
        p.min_replicas,
        u64_json(p.max_node_utilization),
        p.max_concurrent_actions,
        p.max_actions_per_node,
        p.phase_timeout_ms,
        p.measurement_settle_ms,
        p.measurement_deadline_ms,
        u64_json(p.outcome_noise_floor)
    ));
    out.push(format!("silence={}", s.silence_budget_ms));
    out.push(format!("probe={}", s.probe_timeout_ms));
    out.push(format!("capacity={}", s.placement_capacity));
    out.push(format!(
        "store={}",
        s.placement_store.as_ref().map(|id| id.0.clone()).unwrap_or("-".into())
    ));
    out.push(format!("drain={}", s.drain_ms));
    out.push(format!("cooldown={}", s.decision_cooldown_ms));
    out.join(" ")
}

/// A config case line: `{"text": <config file text>, "env": [[name, value], ...]}`.
/// With `debug`, a resolved Settings prints as its (redacting) Debug.
fn config_case(line: &str, debug: bool) -> String {
    let case: serde_json::Value = serde_json::from_str(line).expect("case json");
    let text = case["text"].as_str().expect("text");
    let pairs: Vec<(String, String)> = case["env"]
        .as_array()
        .map(|a| {
            a.iter()
                .map(|p| {
                    (
                        p[0].as_str().unwrap().to_string(),
                        p[1].as_str().unwrap().to_string(),
                    )
                })
                .collect()
        })
        .unwrap_or_default();
    let refs: Vec<(&str, &str)> = pairs.iter().map(|(a, b)| (a.as_str(), b.as_str())).collect();
    let file: ConfigFile = match serde_json::from_str(text) {
        Ok(file) => file,
        Err(error) => return format!("PARSE\t{error}"),
    };
    match Settings::resolve(file, &MapEnv::of(&refs)) {
        Ok(settings) if debug => format!("OK\t{settings:?}"),
        Ok(settings) => format!("OK\t{}", settings_digest(&settings)),
        Err(error) => format!("ERR\t{error}"),
    }
}

// ---------------------------------------------------------------- status

fn opt_u64(v: &Value) -> Option<u64> {
    v.as_u64()
}

fn opt_str(v: &Value) -> Option<String> {
    v.as_str().map(str::to_string)
}

fn text(v: &Value) -> String {
    v.as_str().expect("a string").to_string()
}

fn u(v: &Value) -> u64 {
    v.as_u64().expect("a u64")
}

fn list<T>(v: &Value, f: impl Fn(&Value) -> T) -> Vec<T> {
    v.as_array().expect("an array").iter().map(f).collect()
}

/// Status has no Deserialize; this builds one field for field from its own
/// JSON shape, so the crate's Serialize is what writes it back.
fn status_from(v: &Value) -> Status {
    Status {
        version: Box::leak(text(&v["version"]).into_boxed_str()),
        started_at_ms: u(&v["started_at_ms"]),
        snapshot_at_ms: u(&v["snapshot_at_ms"]),
        clock_ms: u(&v["clock_ms"]),
        cycles: u(&v["cycles"]),
        draining: v["draining"].as_bool().unwrap(),
        data_listen: text(&v["data_listen"]),
        admin_listen: text(&v["admin_listen"]),
        routing_generation: u(&v["routing_generation"]),
        placement_generation: u(&v["placement_generation"]),
        telemetry_source: text(&v["telemetry_source"]),
        observations: u(&v["observations"]) as usize,
        profiles: u(&v["profiles"]) as usize,
        hot_cells: u(&v["hot_cells"]) as usize,
        keyspace: KeyspaceStatus {
            rules: list(&v["keyspace"]["rules"], text),
            fallback: opt_str(&v["keyspace"]["fallback"]),
            spans_one_place: v["keyspace"]["spans_one_place"].as_bool().unwrap(),
        },
        backends: list(&v["backends"], |b| BackendStatus {
            id: text(&b["id"]),
            url: text(&b["url"]),
            region: text(&b["region"]),
            availability: text(&b["availability"]),
            health: text(&b["health"]),
            last_probe: text(&b["last_probe"]),
            last_probe_at_ms: opt_u64(&b["last_probe_at_ms"]),
            silence_ms: opt_u64(&b["silence_ms"]),
            heartbeats: u(&b["heartbeats"]),
            telemetry: b["telemetry"].as_bool().unwrap(),
            last_sample: opt_str(&b["last_sample"]),
        }),
        placements: list(&v["placements"], |p| PlacementStatus {
            shard: u(&p["shard"]),
            x: u(&p["x"]) as u8,
            y: u(&p["y"]) as u8,
            holder: text(&p["holder"]),
            region: text(&p["region"]),
            migration: if p["migration"].is_null() {
                None
            } else {
                let m = &p["migration"];
                Some(MigrationStatus {
                    phase: text(&m["phase"]),
                    source: text(&m["source"]),
                    destination: text(&m["destination"]),
                    read_owner: text(&m["read_owner"]),
                    write_fenced: m["write_fenced"].as_bool().unwrap(),
                    has_cut_over: m["has_cut_over"].as_bool().unwrap(),
                })
            },
        }),
        in_flight: list(&v["in_flight"], |a| ActionStatus {
            id: u(&a["id"]),
            shard: u(&a["shard"]),
            x: u(&a["x"]) as u8,
            y: u(&a["y"]) as u8,
            action: text(&a["action"]),
            mechanism: text(&a["mechanism"]),
            state: text(&a["state"]),
            phase: opt_str(&a["phase"]),
            fraction: a["fraction"].as_f64(),
            source: text(&a["source"]),
            destination: opt_str(&a["destination"]),
            admitted_at_ms: u(&a["admitted_at_ms"]),
            has_cut_over: a["has_cut_over"].as_bool().unwrap(),
            bytes_copied: u(&a["bytes_copied"]),
            resident_bytes: opt_u64(&a["resident_bytes"]),
        }),
        decisions: DecisionCounters {
            proposed: u(&v["decisions"]["proposed"]),
            admitted: u(&v["decisions"]["admitted"]),
            refused: u(&v["decisions"]["refused"]),
            last_refusal: opt_str(&v["decisions"]["last_refusal"]),
        },
        placement_store: StoreStatus {
            configured: opt_str(&v["placement_store"]["configured"]),
            state: text(&v["placement_store"]["state"]),
            last_error: opt_str(&v["placement_store"]["last_error"]),
            writes: u(&v["placement_store"]["writes"]),
        },
        movers: MoverStatus {
            copying: list(&v["movers"]["copying"], |c| CopyingStatus {
                id: u(&c["id"]),
                destination: text(&c["destination"]),
            }),
            refused: list(&v["movers"]["refused"], |r| RefusedCopy {
                id: u(&r["id"]),
                reason: text(&r["reason"]),
            }),
        },
        history: serde_json::from_value(v["history"].clone()).expect("history"),
    }
}

/// A status line: the status JSON, then what each admin route answers with
/// it (admin.rs's own expressions), tab-separated.
fn status_case(line: &str) -> String {
    let v: Value = serde_json::from_str(line).expect("status json");
    let status = status_from(&v);
    let routing = serde_json::json!({
        "routing_generation": status.routing_generation,
        "placement_generation": status.placement_generation,
        "snapshot_at_ms": status.snapshot_at_ms,
        "placements": status.placements,
    });
    [
        serde_json::to_string(&status).unwrap(),
        serde_json::to_string(&status.backends).unwrap(),
        serde_json::to_string(&status.placements).unwrap(),
        serde_json::to_string(&routing).unwrap(),
        serde_json::to_string(&status.in_flight).unwrap(),
        serde_json::to_string(&status.history).unwrap(),
    ]
    .join("\t")
}

// ---------------------------------------------------------------- liveness, telemetry

fn block_on<F: std::future::Future>(future: F) -> F::Output {
    tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .expect("a tokio runtime")
        .block_on(future)
}

/// `kind|status|error` -> `reached|healthy|describe`.
fn probe_case(line: &str) -> String {
    let parts: Vec<&str> = line.splitn(3, '|').collect();
    let status: u16 = parts[1].parse().unwrap_or(0);
    let probe = match parts[0] {
        "Serving" => Probe::Serving { status },
        "Answering" => Probe::Answering { status },
        _ => Probe::Unreachable { error: parts[2].to_string() },
    };
    format!("{}|{}|{}", probe.reached(), probe.healthy(), probe.describe())
}

/// `{"timeout_ms": N, "targets": [[id, url], ...]}` -> one real sweep,
/// `id=describe` per target, `;`-separated.
fn sweep_case(line: &str) -> String {
    let case: Value = serde_json::from_str(line).expect("sweep json");
    let targets = list(&case["targets"], |t| ProbeTarget {
        id: fabric_core::DbmsId::new(text(&t[0])),
        url: text(&t[1]),
    });
    let prober = LivenessProber::new(targets, Duration::from_millis(u(&case["timeout_ms"])))
        .expect("a prober");
    let swept = block_on(prober.sweep());
    swept
        .iter()
        .map(|(id, probe)| format!("{}={}", id.0, probe.describe()))
        .collect::<Vec<_>>()
        .join(";")
}

/// A config case line (see config_case) resolved, then the live telemetry
/// source built from it (lib.rs facetql_telemetry) and sampled once:
/// `describe|messages|id=note;...`.
fn telemetry_case(line: &str) -> String {
    let case: Value = serde_json::from_str(line).expect("case json");
    let pairs: Vec<(String, String)> = list(&case["env"], |p| (text(&p[0]), text(&p[1])));
    let refs: Vec<(&str, &str)> = pairs.iter().map(|(a, b)| (a.as_str(), b.as_str())).collect();
    let file: ConfigFile = serde_json::from_str(case["text"].as_str().unwrap()).expect("config");
    let settings = match Settings::resolve(file, &MapEnv::of(&refs)) {
        Ok(settings) => settings,
        Err(error) => return format!("ERR\t{error}"),
    };
    let factory = fabric_daemon::facetql_telemetry(&settings);
    block_on(async move {
        let mut source = factory().expect("a telemetry source");
        let sample = source.sample().await;
        let notes: Vec<String> = sample
            .notes
            .iter()
            .map(|(id, note)| format!("{}={}", id.0, note))
            .collect();
        format!("{}|{}|{}", source.describe(), sample.messages.len(), notes.join(";"))
    })
}

// ---------------------------------------------------------------- admin

/// One request through the real admin router (admin.rs), the control loop
/// answering as the case says. `{"token", "status": "<status json>",
/// "reply": {"ok": m} | {"err": m} | "stopped" | "dropped", "method",
/// "path", "headers": [[k, v]], "body"}` ->
/// `status|content-type|allow|<body as a JSON string>|<request the loop saw>`.
fn admin_case(line: &str) -> String {
    use tower::ServiceExt;

    let case: Value = serde_json::from_str(line).expect("admin json");
    let snapshot: Value = serde_json::from_str(case["status"].as_str().unwrap()).expect("status json");
    let status = std::sync::Arc::new(fabric_daemon::status::StatusHandle::new(status_from(&snapshot)));
    let (requests, mut receiver) =
        tokio::sync::mpsc::unbounded_channel::<fabric_daemon::control::ControlRequest>();

    block_on(async move {
        let reply = case["reply"].clone();
        let seen = std::sync::Arc::new(std::sync::Mutex::new(String::from("-")));
        let saw = seen.clone();

        if reply == "stopped" {
            drop(receiver);
        } else {
            tokio::spawn(async move {
                use fabric_daemon::control::ControlRequest;
                let Some(request) = receiver.recv().await else {
                    return;
                };
                let answer: Result<String, String> = match &reply {
                    Value::Object(map) if map.contains_key("ok") => Ok(text(&map["ok"])),
                    Value::Object(map) => Err(text(&map["err"])),
                    _ => Err(String::new()),
                };
                let (description, sender) = match request {
                    ControlRequest::Transfer { id, atoms_copied, bytes_copied, resident_bytes, reply } => (
                        format!("Transfer {} {} {} {:?}", id.0, atoms_copied, bytes_copied, resident_bytes),
                        Some(reply),
                    ),
                    ControlRequest::Abort { id, reply } => (format!("Abort {}", id.0), Some(reply)),
                    ControlRequest::Copy { id, .. } => (format!("Copy {}", id.0), None),
                };
                *saw.lock().unwrap() = description;
                if let Some(sender) = sender {
                    if reply != "dropped" {
                        let _ = sender.send(answer);
                    }
                }
            });
        }

        let router = fabric_daemon::admin::router(fabric_daemon::admin::AdminState::new(
            status,
            text(&case["token"]),
            requests,
        ));

        let mut request = axum::http::Request::builder()
            .method(case["method"].as_str().unwrap())
            .uri(case["path"].as_str().unwrap());
        for header in case["headers"].as_array().unwrap() {
            request = request.header(header[0].as_str().unwrap(), header[1].as_str().unwrap());
        }
        let request = request
            .body(axum::body::Body::from(text(&case["body"])))
            .expect("a request");

        let response = router.oneshot(request).await.expect("a response");
        let code = response.status().as_u16();
        let header = |name: &str| {
            response
                .headers()
                .get(name)
                .and_then(|v| v.to_str().ok())
                .unwrap_or("-")
                .to_string()
        };
        let content_type = header("content-type");
        let allow = header("allow");
        let body = axum::body::to_bytes(response.into_body(), usize::MAX)
            .await
            .expect("a body");
        let body = String::from_utf8_lossy(&body).to_string();
        tokio::task::yield_now().await;
        let seen = seen.lock().unwrap().clone();
        format!(
            "{code}|{content_type}|{allow}|{}|{seen}",
            serde_json::to_string(&body).unwrap()
        )
    })
}

/// tests/mover.rs's loaded-cell sampling with the crate's own poller and
/// optimizer: `base|token|seconds` -> the hottest profile seen polling every
/// 500 ms (after a baseline), stopping once one is hot:
/// `pressure|hot|cpu|queue|write_latency_us|write_ratio|action`.
fn loaded_poll_case(line: &str) -> String {
    use fabric_facetql::poller::{PollOutcome, PollTarget, TelemetryPoller};
    use fabric_facetql::FacetqlEndpoint;
    use fabric_optimizer::WorkloadOptimizer;
    use fabric_topology::TopologyRegistry;
    use fabric_workload::WorkloadProfile;

    let parts: Vec<&str> = line.split('|').collect();
    let (base, token, seconds) = (parts[0], parts[1], parts[2].parse::<u64>().unwrap());
    let shard = 9u64;
    let cell = fabric_core::Coordinate::new(0, 0);
    let endpoint = FacetqlEndpoint::new(fabric_core::DbmsId::new("loaded-instance"), base, token).unwrap();
    block_on(async move {
        let mut poller = TelemetryPoller::new(vec![PollTarget::new(endpoint, shard, cell, "us-east")]).unwrap();
        poller.poll_once().await;
        let mut hottest: Option<WorkloadProfile> = None;
        let deadline = std::time::Instant::now() + Duration::from_secs(seconds);
        while std::time::Instant::now() < deadline {
            tokio::time::sleep(Duration::from_millis(500)).await;
            for (_, outcome) in poller.poll_once().await {
                let PollOutcome::Sampled(batch) = outcome else { continue };
                for sample in &batch.samples {
                    let profile = WorkloadProfile::from_metrics(shard, cell, sample.metrics());
                    if hottest.as_ref().is_none_or(|h| profile.pressure_score > h.pressure_score) {
                        hottest = Some(profile);
                    }
                }
            }
            if hottest.as_ref().is_some_and(|p| p.is_hot()) {
                break;
            }
        }
        let Some(h) = hottest else { return "no sample".to_string() };
        let mut registry = TopologyRegistry::new();
        registry.place(fabric_core::DbmsId::new("loaded-instance"), &fabric_core::Shard::new(shard, "us-east"), cell, "us-east");
        let decision = WorkloadOptimizer::default().optimize(&h, &registry);
        format!("{}|{}|{}|{}|{}|{}|{:?}", h.pressure_score, h.is_hot(), h.cpu_utilization, h.queue_depth, h.write_latency_us, h.write_ratio, decision.action)
    })
}

pub fn run(mode: &str) {
    match mode {
        "daemon-config" => {
            for line in stdin_lines() {
                println!("{}", config_case(&line, false));
            }
        }
        "daemon-status" => {
            for line in stdin_lines() {
                println!("{}", status_case(&line));
            }
        }
        "daemon-probe" => {
            for line in stdin_lines() {
                println!("{}", probe_case(&line));
            }
        }
        "daemon-sweep" => {
            for line in stdin_lines() {
                println!("{}", sweep_case(&line));
            }
        }
        "daemon-telemetry" => {
            for line in stdin_lines() {
                println!("{}", telemetry_case(&line));
            }
        }
        "daemon-control" => crate::daemon_control::run(mode),
        "daemon-loaded-poll" => {
            for line in stdin_lines() {
                println!("{}", loaded_poll_case(&line));
            }
        }
        "daemon-admin" => {
            for line in stdin_lines() {
                println!("{}", admin_case(&line));
            }
        }
        "daemon-settings-debug" => {
            for line in stdin_lines() {
                println!("{}", config_case(&line, true));
            }
        }
        other => {
            eprintln!("fabric_check: unknown daemon mode {other:?}");
            std::process::exit(2);
        }
    }
}

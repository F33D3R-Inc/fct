//! The Rust half of selfhost's fabric wire checks.
//!
//! `fabric_check <mode>` reads its input one item per stdin line and prints
//! one line per item, in exactly the text form the self-hosted port prints
//! for the same input, so the Go test compares the two line by line. Modes
//! are independent `match` arms; new checks append a mode.

mod runtime_cases;
mod fql;
mod daemon;
mod daemon_control;
mod leafb;

use std::io::BufRead;

use fabric_core::{Coordinate, DbmsId, Shard};
use fabric_protocol::{
    FabricMessage, FabricResponse, NodeHeartbeat, NodeRegistration, Placement, ProtocolServer,
    TelemetryBatch, TelemetrySample, TopologyReport, WorkloadObservation,
};
use fabric_telemetry::CellWorkloadMetrics;

fn stdin_lines() -> Vec<String> {
    std::io::stdin()
        .lock()
        .lines()
        .map(|line| line.expect("stdin"))
        .collect()
}

/// A decoded message as `<message json>|<response json>`: the message
/// re-encoded by serde_json, then what `ProtocolServer::handle` answers,
/// encoded by `ProtocolServer::encode`. `ERR|<ProtocolError Display>` when
/// decoding fails.
fn decode_line(line: &str) -> String {
    match ProtocolServer::decode(line.as_bytes()) {
        Ok(message) => {
            let json = serde_json::to_string(&message).expect("encode message");
            let server = ProtocolServer::new("127.0.0.1:7700".parse().unwrap());
            let response = server.handle(message);
            let encoded = ProtocolServer::encode(&response).expect("encode response");
            format!("{json}|{}", String::from_utf8(encoded).unwrap())
        }
        Err(error) => format!("ERR|{error}"),
    }
}

fn cell(x: u8, y: u8, z: u8, q: u8, r: f64, w: f64, br: f64, bw: f64) -> CellWorkloadMetrics {
    CellWorkloadMetrics {
        x,
        y,
        z,
        q,
        reads_per_second: r,
        writes_per_second: w,
        bytes_read_per_second: br,
        bytes_written_per_second: bw,
    }
}

fn sample(x: u8, y: u8, ops: f64, cells: Vec<CellWorkloadMetrics>, partial: bool) -> TelemetrySample {
    TelemetrySample {
        coordinate: Coordinate::new(x, y),
        operations_per_second: ops,
        read_ratio: 0.7,
        write_ratio: 0.30000000000000004,
        read_latency_us: 1234.5678,
        write_latency_us: 1e-7,
        cpu_utilization: 0.1,
        memory_utilization: 1.0 / 3.0,
        queue_depth: u64::MAX,
        cell_breakdown: cells,
        cell_breakdown_partial: partial,
    }
}

/// Messages built through the real constructors and types, one per line.
fn message_samples() -> Vec<FabricMessage> {
    vec![
        FabricMessage::RegisterNode(NodeRegistration::new(
            DbmsId::new("us-east-db-0"),
            "0.13.0",
            "us-east",
        )),
        FabricMessage::RegisterNode(NodeRegistration::new(
            DbmsId::new("quote\"back\\slash\nnl\ttab\u{1}\u{7f}é😀"),
            "",
            "r/\u{1f}",
        )),
        FabricMessage::Heartbeat(NodeHeartbeat {
            node_id: DbmsId::new("db-a"),
            timestamp_ms: 1_700_000_000_000,
            healthy: true,
        }),
        FabricMessage::Heartbeat(NodeHeartbeat {
            node_id: DbmsId::new(""),
            timestamp_ms: u64::MAX,
            healthy: false,
        }),
        FabricMessage::Topology(TopologyReport {
            node_id: DbmsId::new("db-1"),
            timestamp_ms: 42,
            placements: vec![
                Placement {
                    coordinate: Coordinate::new(0, 0),
                    dbms_id: DbmsId::new("db-2"),
                    region: "eu-west".into(),
                },
                Placement {
                    coordinate: Coordinate::new(255, 13),
                    dbms_id: DbmsId::new("db-3"),
                    region: "".into(),
                },
            ],
        }),
        FabricMessage::Topology(TopologyReport {
            node_id: DbmsId::new("db-empty"),
            timestamp_ms: 9_223_372_036_854_775_808,
            placements: vec![],
        }),
        FabricMessage::Telemetry(TelemetryBatch {
            timestamp_ms: 500,
            node_id: DbmsId::new("db-5"),
            shard: Shard::new(7, "orders"),
            samples: vec![
                sample(
                    1,
                    2,
                    12.5,
                    vec![
                        cell(0, 1, 2, 3, 0.1, 0.2, 1e21, 5e-324),
                        cell(255, 0, 0, 255, -0.0, f64::MAX, 123456789012345680000.0, 1e-5),
                    ],
                    true,
                ),
                sample(11, 12, 1e16, vec![], false),
                sample(3, 4, 0.0001, vec![], true),
            ],
        }),
        FabricMessage::Telemetry(TelemetryBatch {
            timestamp_ms: 0,
            node_id: DbmsId::new("db-6"),
            shard: Shard::new(u64::MAX, ""),
            samples: vec![],
        }),
        FabricMessage::Workload(WorkloadObservation {
            node_id: "db-4".into(),
            coordinate: Coordinate::new(5, 6),
            timestamp_ms: 77,
            operations_per_second: 9.5,
            read_ratio: 0.5,
            write_ratio: 0.5,
        }),
        FabricMessage::Workload(WorkloadObservation {
            node_id: "db-9".into(),
            coordinate: Coordinate::new(0, 255),
            timestamp_ms: 1,
            operations_per_second: 9007199254740993.0,
            read_ratio: 2.2250738585072014e-308,
            write_ratio: -1.5e300,
        }),
    ]
}

fn response_samples() -> Vec<FabricResponse> {
    vec![
        FabricResponse::Acknowledged,
        FabricResponse::Registered {
            node_id: "db-9".into(),
        },
        FabricResponse::Registered {
            node_id: "q\"\\\u{8}\u{c}\u{0}😀".into(),
        },
        FabricResponse::OptimizationProposal {
            coordinate: "(3,4)".into(),
            action: "replicate -> db-c".into(),
            expected_gain: 1.188,
            estimated_cost: 0.35,
            confidence: 0.99,
        },
        FabricResponse::OptimizationProposal {
            coordinate: "".into(),
            action: "".into(),
            expected_gain: -0.0,
            estimated_cost: 1e100,
            confidence: 0.1 + 0.2,
        },
        FabricResponse::Rejected {
            reason: "no \"reason\"".into(),
        },
    ]
}

fn main() {
    let mode = std::env::args().nth(1).unwrap_or_default();

    match mode.as_str() {
        // Each stdin line is an f64 bit pattern as a signed decimal; prints
        // the value as serde_json writes it.
        "f64-text" => {
            for line in stdin_lines() {
                let bits: i64 = line.trim().parse().expect("bits");
                let value = f64::from_bits(bits as u64);
                println!("{}", serde_json::to_string(&value).unwrap());
            }
        }

        // Each stdin line is JSON text decoded as an f64: its bit pattern as
        // a signed decimal, or ERR|<serde_json error Display>.
        "f64-parse" => {
            for line in stdin_lines() {
                match serde_json::from_slice::<f64>(line.as_bytes()) {
                    Ok(value) => println!("{}", value.to_bits() as i64),
                    Err(error) => println!("ERR|{error}"),
                }
            }
        }

        // Each stdin line is a FabricMessage payload; see decode_line.
        "protocol-decode" => {
            for line in stdin_lines() {
                println!("{}", decode_line(&line));
            }
        }

        // The Rust-built message samples, one serde_json line each.
        "protocol-samples" => {
            for message in message_samples() {
                println!("{}", serde_json::to_string(&message).unwrap());
            }
        }

        // Each stdin line is a FabricResponse: re-encoded, or
        // ERR|<serde_json error Display>.
        "response-decode" => {
            for line in stdin_lines() {
                match serde_json::from_slice::<FabricResponse>(line.as_bytes()) {
                    Ok(response) => println!("{}", serde_json::to_string(&response).unwrap()),
                    Err(error) => println!("ERR|{error}"),
                }
            }
        }

        // fabric-cli's session file: each stdin line is a JSON string
        // literal holding a whole payload (so it may span lines), decoded as
        // serde_json::from_slice::<Vec<FabricMessage>>. Prints
        // OK|<count>|<messages re-encoded> or ERR|<serde_json error Display>.
        "session-decode" => {
            for line in stdin_lines() {
                let payload: String = serde_json::from_str(&line).expect("a JSON string literal");
                match serde_json::from_slice::<Vec<FabricMessage>>(payload.as_bytes()) {
                    Ok(messages) => println!("OK|{}|{}", messages.len(), serde_json::to_string(&messages).unwrap()),
                    Err(error) => println!("ERR|{error}"),
                }
            }
        }

        // The Rust-built response samples, encoded by ProtocolServer::encode.
        "response-samples" => {
            for response in response_samples() {
                let bytes = ProtocolServer::encode(&response).unwrap();
                println!("{}", String::from_utf8(bytes).unwrap());
            }
        }

        // fabric-runtime's #[test]s replayed as digests (no stdin).
        "runtime-cases" => runtime_cases::print_all(),

        // fabric-facetql's wire, client and placement store (fql.rs).
        m if m.starts_with("fql-") => fql::run(m),

        // fabric-daemon: config, status, liveness, control, admin (daemon.rs).
        m if m.starts_with("daemon-") => daemon::run(m),

        // fabric_leaf_b_test.go: EngineStats bodies the fct port wrote (leafb.rs).
        m if m.starts_with("leafb-") => leafb::run(m),

        other => {
            eprintln!("fabric_check: unknown mode {other:?}");
            std::process::exit(2);
        }
    }
}

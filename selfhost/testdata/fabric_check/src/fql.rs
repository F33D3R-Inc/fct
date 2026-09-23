//! The Rust half of selfhost's fabric-facetql checks
//! (fabric_facetql_test.go): the real `fabric_facetql` wire types, client
//! and placement store, driven from the same inputs the fct port
//! (fabric_facetql_{wire,client,placement}.fct) is driven from, printing in
//! the same line forms.
//!
//! Modes (each `fql-*`):
//! * `fql-encode-query|create|tx`: stdin lines are JSON specs of a
//!   Serialize-only value (read exactly as the fct side reads them); prints
//!   serde_json's encoding of the real value built from each.
//! * `fql-decode-node|page|stats`: stdin lines are JSON bodies decoded into
//!   the real Deserialize types; prints the canonical rendering or
//!   `ERR <serde message>`.
//! * `fql-endpoint`: stdin `dbms\turl\ttoken` lines through
//!   `FacetqlEndpoint::new`.
//! * `fql-endpoint-env`: stdin `dbms\turl\tvar` lines through
//!   `FacetqlEndpoint::from_env`.
//! * `fql-error`: stdin `kind\tstatus\ta\tb` lines, one constructed
//!   `FacetqlError` each: variant, implies_unhealthy, Display.
//! * `fql-trace <base_url> <token> <dead_url>`: the fixed client/placement
//!   script `fqlTraceScript` in fabric_facetql_placement.fct also runs, one
//!   result line per step, against whatever server answers at base_url.

use fabric_core::{Coordinate, DbmsId};
use fabric_facetql::wire::{EngineStats, LatencyStats, Node, QueryPage};
use fabric_facetql::{
    CreateNodeRequest, Expect, FacetqlClient, FacetqlEndpoint, FacetqlError, PlacementStore,
    QueryRequest, StoredPlacement, TransactionRequest, TxOperation,
};
use fabric_topology::Placement;
use serde_json::{json, Value};
use std::io::BufRead;

fn lines() -> Vec<String> {
    std::io::stdin()
        .lock()
        .lines()
        .map(|line| line.expect("stdin"))
        .collect()
}

fn opt_str(spec: &Value, key: &str) -> Option<String> {
    spec.get(key).and_then(Value::as_str).map(str::to_string)
}

fn u8_of(spec: &Value, key: &str) -> u8 {
    spec.get(key).and_then(Value::as_u64).unwrap_or(0) as u8
}

fn str_of(spec: &Value, key: &str) -> String {
    opt_str(spec, key).unwrap_or_default()
}

fn bool_of(spec: &Value, key: &str) -> bool {
    spec.get(key).and_then(Value::as_bool).unwrap_or(false)
}

fn non_null(spec: &Value, key: &str) -> Option<Value> {
    spec.get(key).filter(|v| !v.is_null()).cloned()
}

fn spec_query(spec: &Value) -> QueryRequest {
    QueryRequest {
        kind: opt_str(spec, "kind"),
        owner: opt_str(spec, "owner"),
        where_: non_null(spec, "where"),
        item_var: opt_str(spec, "item_var"),
        order: opt_str(spec, "order"),
        desc: bool_of(spec, "desc"),
        after: opt_str(spec, "after"),
        limit: spec.get("limit").and_then(Value::as_u64).map(|n| n as usize),
    }
}

fn spec_create(spec: &Value) -> CreateNodeRequest {
    CreateNodeRequest {
        address: str_of(spec, "address"),
        kind: str_of(spec, "kind"),
        x: u8_of(spec, "x"),
        y: u8_of(spec, "y"),
        z: u8_of(spec, "z"),
        q: u8_of(spec, "q"),
        data: str_of(spec, "data"),
        public: bool_of(spec, "public"),
        if_absent: bool_of(spec, "if_absent"),
    }
}

fn spec_op(spec: &Value) -> TxOperation {
    let address = str_of(spec, "address");
    match str_of(spec, "op").as_str() {
        "insert_node" => TxOperation::InsertNode {
            address,
            kind: str_of(spec, "kind"),
            x: u8_of(spec, "x"),
            y: u8_of(spec, "y"),
            z: u8_of(spec, "z"),
            q: u8_of(spec, "q"),
            data: str_of(spec, "data"),
            public: bool_of(spec, "public"),
        },
        "delete_node" => TxOperation::DeleteNode { address },
        "insert_edge" => TxOperation::InsertEdge {
            from: str_of(spec, "from"),
            to: str_of(spec, "to"),
            kind: str_of(spec, "kind"),
        },
        "delete_edge" => TxOperation::DeleteEdge {
            from: str_of(spec, "from"),
            to: str_of(spec, "to"),
            kind: str_of(spec, "kind"),
        },
        "clear_kind" => TxOperation::ClearKind {
            kind: str_of(spec, "kind"),
        },
        "delete_where" => TxOperation::DeleteWhere {
            kind: str_of(spec, "kind"),
            where_: non_null(spec, "where"),
        },
        _ => {
            let ex = spec.get("expect").cloned().unwrap_or(Value::Null);
            let expect = if let Some(le) = ex.get("le") {
                Expect::AtMost(le.as_f64().unwrap_or(0.0))
            } else if let Some(eq) = ex.get("eq") {
                Expect::Equals(eq.clone())
            } else {
                Expect::Absent(bool_of(&ex, "absent"))
            };
            let set = match spec.get("set") {
                Some(Value::Object(map)) => map.clone(),
                _ => serde_json::Map::new(),
            };
            TxOperation::SetIf {
                address,
                field: str_of(spec, "field"),
                expect,
                set,
            }
        }
    }
}

fn latency_value(l: &LatencyStats) -> Value {
    json!({"count": l.count, "p50_us": l.p50_us, "p99_us": l.p99_us, "max_us": l.max_us})
}

/// Every EngineStats field as one Value (Options as null) — the rendering
/// fqlStatsValue builds on the fct side.
fn stats_value(s: &EngineStats) -> Value {
    let rq = &s.runtime.requests;
    let w = &s.runtime.window;
    let p = &s.runtime.process;
    json!({
        "node_count": s.node_count, "edge_count": s.edge_count, "user_count": s.user_count,
        "history_entries": s.history_entries,
        "kinds": s.kinds.iter().map(|k| json!({"kind": k.kind, "count": k.count})).collect::<Vec<_>>(),
        "reads_total": s.reads_total, "writes_total": s.writes_total,
        "storage": {"page_size": s.storage.page_size, "segments": s.storage.segments,
                    "pages": s.storage.pages, "obsolete_bytes": s.storage.obsolete_bytes},
        "version": s.version,
        "runtime": {
            "uptime_seconds": s.runtime.uptime_seconds,
            "requests": {"total": rq.total, "read": rq.read, "write": rq.write, "excluded": rq.excluded,
                         "unclassified": rq.unclassified, "in_flight": rq.in_flight,
                         "max_concurrent": rq.max_concurrent, "write_queue_depth": rq.write_queue_depth,
                         "write_queue_contended_total": rq.write_queue_contended_total},
            "window": {"duration_ms": w.duration_ms, "age_ms": w.age_ms, "cpu_utilization": w.cpu_utilization,
                       "read_latency": latency_value(&w.read_latency),
                       "write_latency": latency_value(&w.write_latency)},
            "process": {"cpu_seconds_total": p.cpu_seconds_total, "cpu_cores": p.cpu_cores,
                        "resident_bytes": p.resident_bytes, "memory_limit_bytes": p.memory_limit_bytes,
                        "memory_limit_source": p.memory_limit_source,
                        "memory_utilization": p.memory_utilization}
        },
        "cells": {
            "capacity": s.cells.capacity, "tracked": s.cells.tracked,
            "overflow_reads": s.cells.overflow_reads, "overflow_writes": s.cells.overflow_writes,
            "unattributed_writes": s.cells.unattributed_writes,
            "cells": s.cells.cells.iter().map(|c| json!({"x": c.x, "y": c.y, "z": c.z, "q": c.q,
                "reads": c.reads, "writes": c.writes, "bytes_read": c.bytes_read,
                "bytes_written": c.bytes_written})).collect::<Vec<_>>()
        }
    })
}

fn variant(e: &FacetqlError) -> &'static str {
    match e {
        FacetqlError::Configuration(_) => "Configuration",
        FacetqlError::Transport(_) => "Transport",
        FacetqlError::Unauthorized { .. } => "Unauthorized",
        FacetqlError::PreconditionFailed(_) => "PreconditionFailed",
        FacetqlError::Conflict(_) => "Conflict",
        FacetqlError::NotFound(_) => "NotFound",
        FacetqlError::Status { .. } => "Status",
        FacetqlError::Decode { .. } => "Decode",
    }
}

/// `ERR <variant> <implies_unhealthy> <Display>` — fqlErrLine.
fn err_line(e: &FacetqlError) -> String {
    format!("ERR {} {} {}", variant(e), e.implies_unhealthy(), e)
}

fn nodes_json(nodes: &[Node]) -> String {
    serde_json::to_string(nodes).unwrap()
}

fn stored_line(s: &StoredPlacement) -> String {
    format!(
        "{} v{} {} {} {}",
        s.address(),
        s.version,
        s.placement.dbms_id.0,
        s.placement.shard_id,
        s.placement.region
    )
}

fn placement(shard_id: u64, dbms: &str, region: &str, x: u8, y: u8) -> Placement {
    Placement {
        dbms_id: DbmsId::new(dbms),
        shard_id,
        coordinate: Coordinate::new(x, y),
        region: region.to_string(),
    }
}

fn where_value() -> Value {
    json!({"op": "<", "left": "x", "right": 1})
}

async fn trace(base: &str, token: &str, dead: &str) {
    let endpoint = FacetqlEndpoint::new(DbmsId::new("db-trace"), base, token).unwrap();
    let client = FacetqlClient::new(endpoint);
    let mut out: Vec<String> = Vec::new();

    let stats = |r: Result<EngineStats, FacetqlError>| match r {
        Ok(s) => stats_value(&s).to_string(),
        Err(e) => err_line(&e),
    };
    let nodes = |r: Result<Vec<Node>, FacetqlError>| match r {
        Ok(n) => nodes_json(&n),
        Err(e) => err_line(&e),
    };
    let page = |r: Result<QueryPage, FacetqlError>| match r {
        Ok(p) => format!("{}|{}|{}", nodes_json(&p.nodes), p.next, p.has_more()),
        Err(e) => err_line(&e),
    };
    let unit = |r: Result<(), FacetqlError>| match r {
        Ok(()) => "Ok".to_string(),
        Err(e) => err_line(&e),
    };

    out.push(stats(client.stats().await));
    out.push(stats(client.stats().await));
    out.push(nodes(client.list_nodes(Some("__fabric_placement"), None, Some(500), Some(0)).await));
    out.push(nodes(client.list_nodes(None, Some("o w&n=r/é+*"), None, None).await));
    out.push(nodes(client.list_nodes(None, None, None, None).await));
    let full = QueryRequest {
        kind: Some("Post".into()),
        owner: Some("alice".into()),
        where_: Some(where_value()),
        item_var: Some("p".into()),
        order: Some("title".into()),
        desc: true,
        after: Some("Y3Vyc29y".into()),
        limit: Some(10),
    };
    out.push(page(client.query(&full).await));
    out.push(nodes(client.query_all(&full).await));
    out.push(match client.get_node("p:1 /é?#").await {
        Ok(Some(n)) => nodes_json(&[n]),
        Ok(None) => "None".into(),
        Err(e) => err_line(&e),
    });
    out.push(match client.get_node("gone").await {
        Ok(Some(n)) => nodes_json(&[n]),
        Ok(None) => "None".into(),
        Err(e) => err_line(&e),
    });
    out.push(nodes(client.multiget(&["a".to_string(), "b\"c".to_string()]).await));
    out.push(match client.count(&full).await {
        Ok(n) => n.to_string(),
        Err(e) => err_line(&e),
    });
    out.push(unit(
        client
            .create_node(&CreateNodeRequest {
                address: "x:1".into(),
                kind: "K".into(),
                x: 1,
                y: 2,
                z: 3,
                q: 4,
                data: "{\"a\":1}".into(),
                public: true,
                if_absent: true,
            })
            .await,
    ));
    let mut set = serde_json::Map::new();
    set.insert("version".into(), json!(2));
    set.insert("b".into(), json!([1.5, "x", null]));
    out.push(unit(
        client
            .transaction(vec![
                TxOperation::InsertNode {
                    address: "Entity:1".into(),
                    kind: "Entity".into(),
                    x: 0,
                    y: 0,
                    z: 0,
                    q: 0,
                    data: "{}".into(),
                    public: false,
                },
                TxOperation::DeleteNode { address: "Entity:1".into() },
                TxOperation::InsertEdge { from: "a".into(), to: "b".into(), kind: "rel".into() },
                TxOperation::DeleteEdge { from: "a".into(), to: "b".into(), kind: "rel".into() },
                TxOperation::ClearKind { kind: "Entity".into() },
                TxOperation::DeleteWhere { kind: "Entity".into(), where_: None },
                TxOperation::DeleteWhere { kind: "__session".into(), where_: Some(where_value()) },
                TxOperation::SetIf {
                    address: "__cron:nightly".into(),
                    field: "next_run".into(),
                    expect: Expect::AtMost(1000.0),
                    set: set.clone(),
                },
                TxOperation::SetIf {
                    address: "p:1".into(),
                    field: "version".into(),
                    expect: Expect::version(1),
                    set: set.clone(),
                },
                TxOperation::SetIf {
                    address: "p:1".into(),
                    field: "version".into(),
                    expect: Expect::Absent(true),
                    set,
                },
            ])
            .await,
    ));
    let claim = |r: Result<bool, FacetqlError>| match r {
        Ok(b) => b.to_string(),
        Err(e) => err_line(&e),
    };
    out.push(claim(client.claim("__fabric_placement:7:3:4").await));
    out.push(claim(client.claim("held").await));
    out.push(claim(client.claim("../x y?z").await));
    out.push(stats(client.stats().await));
    out.push(stats(client.stats().await));
    out.push(stats(client.stats().await));
    out.push(match client.get_node("bad").await {
        Ok(Some(n)) => nodes_json(&[n]),
        Ok(None) => "None".into(),
        Err(e) => err_line(&e),
    });
    out.push(page(client.query(&QueryRequest::default()).await));
    out.push(match client.count(&QueryRequest::default()).await {
        Ok(n) => n.to_string(),
        Err(e) => err_line(&e),
    });

    let store = PlacementStore::new(client.clone());
    let created = store.create(&placement(9001, "db-a", "us-east", 1, 2)).await;
    out.push(match &created {
        Ok(s) => stored_line(s),
        Err(e) => err_line(e),
    });
    let v1 = created.unwrap_or(StoredPlacement {
        placement: placement(9001, "db-a", "us-east", 1, 2),
        version: 1,
    });
    let updated = store.update(&v1, &placement(9001, "db-b", "eu-west", 1, 2)).await;
    out.push(match &updated {
        Ok(s) => stored_line(s),
        Err(e) => err_line(e),
    });
    out.push(match store.update(&v1, &placement(9002, "db-b", "eu-west", 1, 2)).await {
        Ok(s) => stored_line(&s),
        Err(e) => err_line(&e),
    });
    let v2 = updated.unwrap_or(StoredPlacement {
        placement: placement(9001, "db-b", "eu-west", 1, 2),
        version: 2,
    });
    out.push(unit(store.remove(&v2).await));
    out.push(match store.load().await {
        Ok(all) => all.iter().map(stored_line).collect::<Vec<_>>().join(","),
        Err(e) => err_line(&e),
    });
    out.push(match store.get(9001, Coordinate::new(1, 2)).await {
        Ok(Some(s)) => stored_line(&s),
        Ok(None) => "None".into(),
        Err(e) => err_line(&e),
    });
    out.push(match store.get(9001, Coordinate::new(5, 5)).await {
        Ok(Some(s)) => stored_line(&s),
        Ok(None) => "None".into(),
        Err(e) => err_line(&e),
    });
    out.push(match store.load().await {
        Ok(all) => all.iter().map(stored_line).collect::<Vec<_>>().join(","),
        Err(e) => err_line(&e),
    });
    out.push(match store.load_registry().await {
        Ok((registry, versions)) => {
            let mut keys: Vec<_> = versions.keys().cloned().collect();
            keys.sort();
            let located = registry
                .locate(9001, Coordinate::new(1, 2))
                .map(|p| format!("{} {}", p.dbms_id.0, p.region))
                .unwrap_or_else(|| "None".into());
            format!(
                "{} {} {}",
                registry.len(),
                keys.iter()
                    .map(|k| format!("{k}=v{}", versions[k].version))
                    .collect::<Vec<_>>()
                    .join(","),
                located
            )
        }
        Err(e) => err_line(&e),
    });

    for _ in 0..2 {
        out.push(match client.open_events(&reqwest::Client::new()).await {
            Ok(response) => format!("Ok {}", response.status().as_u16()),
            Err(e) => err_line(&e),
        });
    }

    let dead_client =
        FacetqlClient::new(FacetqlEndpoint::new(DbmsId::new("db-dead"), dead, token).unwrap());
    out.push(stats(dead_client.stats().await));

    for line in out {
        println!("{line}");
    }
}

fn verdict_line(v: &fabric_runtime::CopyVerdict) -> String {
    match v {
        fabric_runtime::CopyVerdict::Pending => "pending".to_string(),
        fabric_runtime::CopyVerdict::Verified { rows, .. } => format!("verified {rows}"),
        fabric_runtime::CopyVerdict::Failed { reason, .. } => format!("failed {reason}"),
    }
}

/// The real CellMover through fabric_facetql_mover.fct's `moverLiveCopy`
/// scenario, against two live FacetQL instances, printing the same
/// `name=value` lines.
async fn mover_copy(src: &str, dst: &str, token: &str) {
    use fabric_facetql::{CellMover, CellScope, Keyspace, KeyspaceRule, MoverConfig};
    use fabric_routing::RoutingKey;

    let client = |id: &str, base: &str| {
        FacetqlClient::new(FacetqlEndpoint::new(DbmsId::new(id), base, token).unwrap())
    };
    let s = client("src", src);
    let d = client("dst", dst);
    let create = |c: &FacetqlClient, address: String, kind: &str, data: String| {
        let c = c.clone();
        let kind = kind.to_string();
        async move {
            c.create_node(&CreateNodeRequest {
                address,
                kind,
                x: 1,
                y: 2,
                z: 3,
                q: 4,
                data,
                public: false,
                if_absent: false,
            })
            .await
            .is_ok()
        }
    };
    let mut seeded = 0;
    for i in 0..30 {
        if create(&s, format!("Post:{i}"), "Post", format!("{{\"n\":{i}}}")).await {
            seeded += 1;
        }
    }
    for j in 0..5 {
        if create(&s, format!("Session:{j}"), "Session", "{}".to_string()).await {
            seeded += 1;
        }
    }
    let key = |shard: u64| RoutingKey::new(shard, Coordinate::new(0, 0)).unwrap();
    let keyspace = Keyspace::new()
        .with_rule(KeyspaceRule::new("Post", "Post:", key(1)).unwrap())
        .unwrap()
        .with_rule(KeyspaceRule::new("Session", "Session:", key(2)).unwrap())
        .unwrap();
    let scope = CellScope::for_key(&keyspace, key(1));
    let config = MoverConfig { batch_ops: 7, batch_bytes: 2 * 1024 * 1024, page_limit: 10 };
    let mut mover = CellMover::new(s.clone(), d.clone(), scope.clone(), config, reqwest::Client::new());
    let feed = mover.subscribe().await;
    let subscribed = feed.is_ok();
    let feed = feed.unwrap();
    let mut progress = Vec::new();
    let snap = mover.snapshot(&feed, |rows, bytes| progress.push(format!("{rows},{bytes}"))).await;
    let snapshot_err = match &snap {
        Ok(()) => String::new(),
        Err(fabric_facetql::MoverError::Wire { .. }) => "Wire".into(),
        Err(fabric_facetql::MoverError::FeedBroken(_)) => "FeedBroken".into(),
        Err(fabric_facetql::MoverError::Runaway { .. }) => "Runaway".into(),
        Err(fabric_facetql::MoverError::NotIdentical(_)) => "NotIdentical".into(),
    };
    let _early = mover.verify(&feed, 1000).await;
    let w1 = create(&s, "Post:3".into(), "Post", "{\"n\":\"updated\"}".into()).await;
    let w2 = s.transaction(vec![TxOperation::DeleteNode { address: "Post:4".into() }]).await.is_ok();
    let w3 = create(&s, "Post:99".into(), "Post", "{\"n\":99}".into()).await;
    let w4 = create(&s, "Session:0".into(), "Session", "{\"out\":\"of scope\"}".into()).await;
    let mut reconciled = 0;
    let mut tries = 0;
    while feed.counts().0 < 3 && tries < 200 {
        reconciled += mover.catch_up(&feed).await.unwrap_or(0);
        if feed.counts().0 < 3 {
            tokio::time::sleep(std::time::Duration::from_millis(10)).await;
        }
        tries += 1;
    }
    reconciled += mover.catch_up(&feed).await.unwrap_or(0);
    let (observed, applied) = feed.counts();
    let verified = mover.verify(&feed, 2000).await;
    let report = mover.report(&feed, verified.clone());
    let tamper = create(&d, "Post:5".into(), "Post", "{\"n\":\"tampered\"}".into()).await;
    let tampered = mover.verify(&feed, 3000).await;
    drop(feed);
    let edge = s
        .transaction(vec![TxOperation::InsertEdge { from: "Post:1".into(), to: "Post:2".into(), kind: "rel".into() }])
        .await
        .is_ok();
    let mut second = CellMover::new(s.clone(), d.clone(), scope, MoverConfig::default(), reqwest::Client::new());
    let feed2 = second.subscribe().await.unwrap();
    let refused = match second.snapshot(&feed2, |_, _| {}).await {
        Ok(()) => " ".to_string(),
        Err(e @ fabric_facetql::MoverError::NotIdentical(_)) => format!("NotIdentical {e}"),
        Err(e) => format!("other {e}"),
    };

    println!("seeded={seeded}");
    println!("subscribe={subscribed}");
    println!("snapshotErr={snapshot_err}");
    println!("rowsCopied={}", report.rows_copied);
    println!("progress={}", progress.join(";"));
    println!("resident={} {}", report.resident_bytes.is_some(), report.resident_bytes.unwrap_or(0));
    println!("writes={w1},{w2},{w3},{w4}");
    println!("observed={observed} applied={applied} reconciled={reconciled}");
    println!("verified={}", verdict_line(&verified));
    println!(
        "report={} {} {} {}",
        report.rows_copied, report.snapshot_complete, report.observed_writes, report.applied_writes
    );
    println!("tampered={tamper} {}", verdict_line(&tampered));
    println!("edge={edge} {refused}");
}

/// Runs one `fql-*` mode.
pub fn run(mode: &str) {
    match mode {
        "fql-encode-query" => {
            for line in lines() {
                let spec: Value = serde_json::from_str(&line).expect("spec");
                println!("{}", serde_json::to_string(&spec_query(&spec)).unwrap());
            }
        }
        "fql-encode-create" => {
            for line in lines() {
                let spec: Value = serde_json::from_str(&line).expect("spec");
                println!("{}", serde_json::to_string(&spec_create(&spec)).unwrap());
            }
        }
        "fql-encode-tx" => {
            for line in lines() {
                let spec: Value = serde_json::from_str(&line).expect("spec");
                let operations = spec.as_array().expect("ops").iter().map(spec_op).collect();
                println!(
                    "{}",
                    serde_json::to_string(&TransactionRequest { operations }).unwrap()
                );
            }
        }
        "fql-decode-node" => {
            for line in lines() {
                match serde_json::from_str::<Node>(&line) {
                    Ok(node) => println!("{}", serde_json::to_string(&node).unwrap()),
                    Err(e) => println!("ERR {e}"),
                }
            }
        }
        "fql-decode-page" => {
            for line in lines() {
                match serde_json::from_str::<QueryPage>(&line) {
                    Ok(p) => println!("{}|{}|{}", nodes_json(&p.nodes), p.next, p.has_more()),
                    Err(e) => println!("ERR {e}"),
                }
            }
        }
        "fql-decode-stats" => {
            for line in lines() {
                match serde_json::from_str::<EngineStats>(&line) {
                    Ok(s) => println!("{}", stats_value(&s)),
                    Err(e) => println!("ERR {e}"),
                }
            }
        }
        "fql-endpoint" => {
            for line in lines() {
                let f: Vec<&str> = line.split('\t').collect();
                match FacetqlEndpoint::new(DbmsId::new(f[0]), f[1], f[2]) {
                    Ok(e) => println!("OK {e:?}|{}", e.base_url()),
                    Err(e) => println!("ERR {} {e}", variant(&e)),
                }
            }
        }
        "fql-endpoint-env" => {
            for line in lines() {
                let f: Vec<&str> = line.split('\t').collect();
                match FacetqlEndpoint::from_env(DbmsId::new(f[0]), f[1], f[2]) {
                    Ok(e) => println!("OK {e:?}|{}", e.base_url()),
                    Err(e) => println!("ERR {} {e}", variant(&e)),
                }
            }
        }
        "fql-error" => {
            for line in lines() {
                let f: Vec<&str> = line.split('\t').collect();
                let status: u16 = f[1].parse().unwrap_or(0);
                let (a, b) = (f[2].to_string(), f[3].to_string());
                let e = match f[0] {
                    "Configuration" => FacetqlError::Configuration(a),
                    "Transport" => FacetqlError::Transport(a),
                    "Unauthorized" => FacetqlError::Unauthorized { status, body: a },
                    "PreconditionFailed" => FacetqlError::PreconditionFailed(a),
                    "Conflict" => FacetqlError::Conflict(a),
                    "NotFound" => FacetqlError::NotFound(a),
                    "Status" => FacetqlError::Status { status, body: a },
                    _ => FacetqlError::Decode { context: a, message: b },
                };
                println!("{}", err_line(&e));
            }
        }
        "fql-mover" => {
            let args: Vec<String> = std::env::args().collect();
            let rt = tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build()
                .unwrap();
            rt.block_on(mover_copy(&args[2], &args[3], &args[4]));
        }
        "fql-trace" => {
            let args: Vec<String> = std::env::args().collect();
            let rt = tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build()
                .unwrap();
            rt.block_on(trace(&args[2], &args[3], &args[4]));
        }
        other => {
            eprintln!("fabric_check: unknown mode {other:?}");
            std::process::exit(2);
        }
    }
}

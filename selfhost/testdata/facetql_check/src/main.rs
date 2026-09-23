//! The Rust half of selfhost's facetql compatibility checks.
//!
//! Every mode reads its input from the command line or stdin and prints a
//! text summary in exactly the format the self-hosted port prints for the
//! same input, so the Go test compares the two strings directly. Keys come
//! from FACETQL_MASTER_KEY, as they do for the engine itself.

use facetql::crypto;
use facetql::core::user::{Role, UserRecord};
use facetql::storage::binary::{self, TailState, UserOpRecord};
use facetql::core::coordinate::Coordinate;
use facetql::core::edge::{Edge, EdgeId};
use facetql::storage::engine::{Expectation, StorageEngine, TxOperation};
use facetql::storage::recovery;
use facetql::core::aggregate::{AggFunc, AggSpec};
use facetql::storage::reference::{ReferenceDef, ReferentialAction};
use facetql::storage::index::IndexDef;
use facetql::storage::text::TextIndexDef;
use facetql::core::history::HistoryEntry;
use facetql::core::node::Node;
use facetql::storage::btree::{BTree, SeekMode};
use facetql::storage::heap::{HeapRecord, RecordStore};
use facetql::storage::location::RecordLocation;
use std::sync::Arc;
use facetql::storage::catalog::{Catalog, CatalogData, SegmentMeta};
use facetql::storage::checkpoint;
use facetql::storage::page::{Page, PageKind, PAGE_BODY_LEN};
use facetql::storage::pager::Pager;
use facetql::storage::wal::{decode_frame, FrameOutcome};
use std::io::BufRead;
use std::path::Path;

fn kind_byte(k: PageKind) -> u8 {
    match k {
        PageKind::Meta => 1,
        PageKind::Leaf => 2,
        PageKind::Branch => 3,
        PageKind::Heap => 4,
        PageKind::Overflow => 5,
    }
}

fn kind_of(b: u8) -> PageKind {
    match b {
        1 => PageKind::Meta,
        2 => PageKind::Leaf,
        3 => PageKind::Branch,
        5 => PageKind::Overflow,
        _ => PageKind::Heap,
    }
}

fn hex(bs: &[u8]) -> String {
    crypto::encode_hex(bs)
}

fn unhex(s: &str) -> Vec<u8> {
    crypto::decode_hex(s).expect("hex")
}

fn cells(p: &Page) -> String {
    (0..p.slot_count())
        .map(|i| hex(p.cell(i).unwrap()))
        .collect::<Vec<_>>()
        .join(",")
}

/// page.fct's pgScriptText, over the real Page.
fn page_script(script: &str) -> String {
    let mut p = Page::new(PageKind::Heap);
    let mut out = String::new();
    for line in script.split('\n') {
        let w: Vec<&str> = line.split_whitespace().collect();
        if w.is_empty() {
            continue;
        }
        match w[0] {
            "new" => {
                p = Page::new(kind_of(w[1].parse().unwrap()));
                out += "new;";
            }
            "push" => match p.push_cell(&unhex(w[1])) {
                Some(i) => out += &format!("push {i};"),
                None => out += "push full;",
            },
            "insert" => {
                let ok = p.insert_cell(w[1].parse().unwrap(), &unhex(w[2]));
                out += &format!("insert {ok};");
            }
            "replace" => {
                let ok = p.replace_cell(w[1].parse().unwrap(), &unhex(w[2]));
                out += &format!("replace {ok};");
            }
            "remove" => {
                p.remove_cell(w[1].parse().unwrap());
                out += "remove;";
            }
            "compact" => {
                p.compact();
                out += "compact;";
            }
            "extra" => {
                p.set_extra(w[1].parse().unwrap());
                out += "extra;";
            }
            other => panic!("unknown page op {other}"),
        }
    }
    let body = p.encode().to_vec();
    format!(
        "{out}\nkind={} extra={} count={} free={} reclaim={}\ncells={}\nbody={}",
        kind_byte(p.kind()),
        p.extra(),
        p.slot_count(),
        p.free_space(),
        p.reclaimable_space(),
        cells(&p),
        hex(&body)
    )
}

/// page.fct's pgDecodeText.
fn decode_text(body: &[u8]) -> String {
    match Page::decode(body) {
        Ok(p) => format!("kind={} count={} cells={}", kind_byte(p.kind()), p.slot_count(), cells(&p)),
        Err(e) => format!("error: {e}"),
    }
}

fn catalog_text(d: &CatalogData) -> String {
    let segs: Vec<String> = d
        .segments
        .iter()
        .map(|s| format!("{}:{}:{}", s.id, s.pages, s.obsolete_bytes))
        .collect();
    format!(
        "v{} page={} next={} active={} segs={}",
        d.format_version,
        d.page_size,
        d.next_segment,
        d.active_segment,
        segs.join(",")
    )
}

// engine_ops: the engine-script vocabulary through the real StorageEngine;
// each put/del that archives reports the version and time it stamped.
fn engine_ops(engine: &StorageEngine, script: &str) -> String {
    let mut out = String::new();
    for op in script.split(';') {
        let line = op.trim();
        if line.is_empty() {
            continue;
        }
        let w: Vec<&str> = line.splitn(5, ' ').collect();
        let wf: Vec<&str> = line.split_whitespace().collect();
        let before = |addr: &str| engine.history_for(addr).expect("history").len();
        let archived = |addr: &str, n0: usize| -> String {
            let h = engine.history_for(addr).expect("history");
            if h.len() > n0 {
                let last = h.iter().max_by_key(|e| e.version).unwrap();
                format!("ok v={} t={};", last.version, last.archived_at_unix)
            } else {
                "ok;".to_string()
            }
        };
        let result: Result<String, String> = match w[0] {
            "put" => {
                let n0 = before(w[1]);
                let mut nd = Node::new(Coordinate::new(1, 2, 3, 250), w[1].to_string(), w[2].to_string(), w[3].to_string());
                nd.value = 7;
                nd.data = w.get(4).unwrap_or(&"").to_string();
                engine.insert(nd).map(|_| archived(w[1], n0))
            }
            "del" => {
                let n0 = before(w[1]);
                engine.delete(w[1]).map_err(|e| e.to_string()).map(|_| archived(w[1], n0))
            }
            "edge" => engine
                .insert_edge(Edge::new(w[1].to_string(), w[2].to_string(), w[3].to_string(), "alice".to_string()))
                .map(|_| "ok;".to_string()),
            "unedge" => engine
                .delete_edge(&EdgeId { from: w[1].to_string(), to: w[2].to_string(), kind: w[3].to_string() })
                .map(|_| "ok;".to_string()),
            "index" => engine
                .create_index(IndexDef { name: wf[1].to_string(), kind: wf[2].to_string(), field: wf[3].to_string(), unique: wf.get(4) == Some(&"unique") })
                .map(|_| "ok;".to_string()),
            "text" => engine
                .create_text_index(TextIndexDef { name: w[1].to_string(), kind: w[2].to_string(), field: w[3].to_string() })
                .map(|_| "ok;".to_string()),
            "drop" => engine.drop_index(w[1]).map(|_| "ok;".to_string()),
            "ref" => {
                let on_delete = match wf[6] {
                    "cascade" => ReferentialAction::Cascade,
                    "restrict" => ReferentialAction::Restrict,
                    _ => ReferentialAction::SetNull,
                };
                let parent_field = if wf[5] == "-" { None } else { Some(wf[5].to_string()) };
                engine
                    .create_reference(ReferenceDef { name: wf[1].to_string(), kind: wf[2].to_string(), field: wf[3].to_string(), parent_kind: wf[4].to_string(), parent_field, on_delete })
                    .map(|_| "ok;".to_string())
            }
            "unref" => engine.drop_reference(w[1]).map(|_| "ok;".to_string()),
            "user" => {
                let role = if wf[3] == "admin" { Role::Admin } else { Role::User };
                engine
                    .insert_user(UserRecord { token_hash: wf[1].to_string(), owner: wf[2].to_string(), role })
                    .map(|_| "ok;".to_string())
            }
            "revoke" => engine.revoke_user(w[1]).map(|_| "ok;".to_string()),
            "checkpoint" => engine.checkpoint().map_err(|e| e.to_string()).map(|_| "ok;".to_string()),
            "tx" => {
                let mut txops = Vec::new();
                for sub in line[3..].split('~') {
                    let sw: Vec<&str> = sub.trim().split(' ').collect();
                    let admin = |s: &str| s == "admin";
                    let op = match sw[0] {
                        "ins" => {
                            let t = sub.trim();
                            let rest: Vec<&str> = t.splitn(5, ' ').collect();
                            let mut nd = Node::new(Coordinate::new(1, 2, 3, 250), rest[1].to_string(), rest[2].to_string(), rest[3].to_string());
                            nd.value = 7;
                            nd.data = rest.get(4).unwrap_or(&"").to_string();
                            TxOperation::InsertNode(nd)
                        }
                        "del" => TxOperation::DeleteNode(sw[1].to_string()),
                        "edge" => TxOperation::InsertEdge(Edge::new(sw[1].to_string(), sw[2].to_string(), sw[3].to_string(), "alice".to_string())),
                        "unedge" => TxOperation::DeleteEdge { id: EdgeId { from: sw[1].to_string(), to: sw[2].to_string(), kind: sw[3].to_string() }, owner: sw[4].to_string(), is_admin: admin(sw[5]) },
                        "clear" => TxOperation::ClearKind { kind: sw[1].to_string(), owner: sw[2].to_string(), is_admin: admin(sw[3]) },
                        "delwhere" => TxOperation::DeleteWhere { kind: sw[1].to_string(), where_: Some(serde_json::from_str(sw[4]).expect("where")), owner: sw[2].to_string(), is_admin: admin(sw[3]) },
                        "setif" => {
                            let expect = if let Some(n) = sw[5].strip_prefix("atmost:") {
                                Expectation::AtMost(n.parse().unwrap())
                            } else if let Some(j) = sw[5].strip_prefix("eq:") {
                                Expectation::Equals(serde_json::from_str(j).unwrap())
                            } else {
                                Expectation::Absent
                            };
                            let set: serde_json::Map<String, serde_json::Value> = serde_json::from_str(sw[6]).unwrap();
                            TxOperation::SetIf { address: sw[1].to_string(), field: sw[2].to_string(), expect, set, owner: sw[3].to_string(), is_admin: admin(sw[4]) }
                        }
                        other => panic!("unknown tx op {other}"),
                    };
                    txops.push(op);
                }
                engine.execute_transaction(txops).map_err(|e| e.to_string()).map(|_| "ok;".to_string())
            }
            "claim" => engine.claim(wf[1], wf[2]).map_err(|e| e.to_string()).map(|_| "ok;".to_string()),
            "seq" => engine.sequence_next(wf[1], wf[2].parse().unwrap(), wf[3], wf[4] == "admin").map(|s| format!("ok start={s};")),
            "iwe" => {
                let rest: Vec<&str> = line.splitn(7, ' ').collect();
                let mut nd = Node::new(Coordinate::new(1, 2, 3, 250), rest[1].to_string(), rest[2].to_string(), rest[3].to_string());
                nd.value = 7;
                nd.data = rest.get(6).unwrap_or(&"").to_string();
                let targets: Vec<(String, String)> = if rest[5] == "-" {
                    Vec::new()
                } else {
                    rest[5].split(',').map(|p| {
                        let (a, b) = p.split_once(':').unwrap();
                        (a.to_string(), b.to_string())
                    }).collect()
                };
                let n = targets.len();
                engine.insert_with_edges(nd, targets, rest[4] == "admin").map_err(|(e, _)| e).map(|_| format!("ok edges={n};"))
            }
            other => panic!("unknown engine op {other}"),
        };
        match result {
            Ok(s) => out += &s,
            Err(e) => out += &format!("error: {e};"),
        }
    }
    out
}

fn node_text(n: &Node) -> String {
    let vis = match n.visibility {
        facetql::core::node::Visibility::Public => "public",
        facetql::core::node::Visibility::Private => "private",
    };
    format!("{}|{}|{}|{}|{}|{}|{}", n.address, n.kind, n.owner, n.value, vis, n.claimed_by.clone().unwrap_or_else(|| "-".to_string()), n.data)
}

fn nodes_text(ns: &[Node]) -> String {
    format!("[{}]", ns.iter().map(node_text).collect::<Vec<_>>().join(","))
}

// archive_times drops the clock from an engine session's archive reports.
fn archive_times(out: &str) -> String {
    let mut s = String::new();
    for part in out.split(';') {
        match part.find(" t=") {
            Some(at) => s += &part[..at],
            None => s += part,
        }
        s += ";";
    }
    s.pop();
    s
}

fn main() {
    let args: Vec<String> = std::env::args().collect();
    let mode = args.get(1).map(String::as_str).unwrap_or("");
    match mode {
        // fqindex.fct's ixScriptText through the real StorageEngine over
        // $HOME/.facetql: each put/del that archives reports the version
        // and time it stamped, so the port can replay the same ones.
        "engine-script" => {
            let engine = StorageEngine::load().expect("load");
            let out = engine_ops(&engine, &args[2]);
            drop(engine);
            let c = Catalog::open().expect("catalog");
            println!("{out}|{}", c.with(catalog_text));
        }
        // fqengine.fct's enScriptText: open as Database::new does (load,
        // then WAL recovery), run the ops, and stop without a checkpoint —
        // as a crash would — unless the script asks for one.
        "engine-wal" => {
            let mut engine = StorageEngine::load().expect("load");
            if let Err(e) = recovery::recover(&mut engine) {
                println!("error: {e}");
                return;
            }
            let out = engine_ops(&engine, &args[2]);
            drop(engine);
            println!("{out}|checkpoint={}", checkpoint::read().expect("checkpoint"));
        }
        // fqengine.fct's enReportText: open as Database::new does (load,
        // recovery, compact_user_log), run the ops, then report stats
        // (without the process-level runtime section), the users, the
        // declared indexes and references, the user log's record count,
        // and one change scan per `after:limit:owner:admin` in args[3].
        "engine-report" => {
            let mut engine = StorageEngine::load().expect("load");
            if let Err(e) = recovery::recover(&mut engine) {
                println!("error: {e}");
                return;
            }
            engine.compact_user_log().expect("compact");
            let out = engine_ops(&engine, &args[2]);
            println!("{}", archive_times(&out));
            let mut stats = serde_json::to_value(engine.stats().expect("stats")).unwrap();
            stats.as_object_mut().unwrap().remove("runtime");
            println!("{stats}");
            let mut users: Vec<String> = engine.list_users().iter().map(|u| format!("{}:{:?}", u.owner, u.role)).collect();
            users.sort();
            println!("{}", users.join(","));
            println!("{}", serde_json::to_string(&engine.list_all_indexes()).unwrap());
            println!("{}", serde_json::to_string(&engine.list_references()).unwrap());
            let log = binary::read_all_records::<UserOpRecord>(&binary::users_path()).expect("users log");
            println!("userlog={}", log.len());
            for spec in args.get(3).map(String::as_str).unwrap_or("").split(';').filter(|s| !s.is_empty()) {
                let f: Vec<&str> = spec.split(':').collect();
                match facetql::storage::changes::scan(f[0].parse().unwrap(), f[1].parse().unwrap(), f[2], f[3] == "admin") {
                    Ok(page) => println!("200 {}", serde_json::to_string(&page).unwrap()),
                    Err(facetql::storage::changes::ScanError::TooOld(t)) => println!("410 {t}"),
                    Err(facetql::storage::changes::ScanError::Log(e)) => println!("500 storage error: {e}"),
                }
            }
        }
        // fqengine.fct's enReadText: open (load + recovery), then each read.
        "engine-read" => {
            let mut engine = StorageEngine::load().expect("load");
            if let Err(e) = recovery::recover(&mut engine) {
                println!("error: {e}");
                return;
            }
            for op in args[2].split(';') {
                let line = op.trim();
                if line.is_empty() {
                    continue;
                }
                let w: Vec<&str> = line.split_whitespace().collect();
                let opt = |s: &str| if s == "-" { None } else { Some(s.to_string()) };
                let res = match w[0] {
                    "get" => match engine.get(w[1]) {
                        Ok(Some(n)) => node_text(&n),
                        Ok(None) => "none".to_string(),
                        Err(e) => format!("error: {e}"),
                    },
                    "multi" => {
                        let addrs: Vec<String> = w[1].split(',').map(|s| s.to_string()).collect();
                        let req = opt(w[2]);
                        match engine.multi_get(&addrs, req.as_deref()) {
                            Ok(ns) => nodes_text(&ns),
                            Err(e) => format!("error: {e}"),
                        }
                    }
                    "history" => match engine.history_for(w[1]) {
                        Ok(hs) => format!("[{}]", hs.iter().map(|h| format!("v{}:{}", h.version, node_text(&h.node))).collect::<Vec<_>>().join(",")),
                        Err(e) => format!("error: {e}"),
                    },
                    "from" | "to" => {
                        let r = if w[0] == "from" { engine.edges_from(w[1]) } else { engine.edges_to(w[1]) };
                        match r {
                            Ok(es) => format!("[{}]", es.iter().map(|x| format!("{}>{}:{}@{}", x.from, x.to, x.kind, x.owner)).collect::<Vec<_>>().join(",")),
                            Err(e) => format!("error: {e}"),
                        }
                    }
                    "owner" => {
                        let req = opt(w[2]);
                        match engine.nodes_by_owner(w[1], req.as_deref()) {
                            Ok(ns) => nodes_text(&ns),
                            Err(e) => format!("error: {e}"),
                        }
                    }
                    "query" => {
                        let (k, o, r) = (opt(w[1]), opt(w[2]), opt(w[3]));
                        match engine.query(k.as_deref(), o.as_deref(), r.as_deref(), w[4].parse().unwrap(), w[5].parse().unwrap()) {
                            Ok(ns) => nodes_text(&ns),
                            Err(e) => format!("error: {e}"),
                        }
                    }
                    other => panic!("unknown read op {other}"),
                };
                println!("{res}");
            }
        }
        // fqquery.fct's qQueryText: one request per stdin line —
        // `op|kind|owner|requester|where|itemVar|order|desc|after|limit|
        // offset|groupBy|values|func|field`, `-` for none.
        "engine-query" => {
            let mut engine = StorageEngine::load().expect("load");
            if let Err(e) = recovery::recover(&mut engine) {
                println!("error: {e}");
                return;
            }
            let mut prev_next = String::new();
            for line in std::io::stdin().lock().lines() {
                let line = line.unwrap();
                if line.trim().is_empty() {
                    continue;
                }
                let f: Vec<&str> = line.split('\t').collect();
                let opt = |s: &str| if s == "-" { None } else { Some(s.to_string()) };
                let (kind, owner, requester) = (opt(f[1]), opt(f[2]), opt(f[3]));
                let pred: Option<facetql::core::predicate::Expr> =
                    opt(f[4]).map(|j| serde_json::from_str(&j).expect("predicate json"));
                if let Some(p) = &pred {
                    if let Err(e) = facetql::core::predicate::validate(p) {
                        println!("error: {e}");
                        continue;
                    }
                }
                let res = match f[0] {
                    "where" => {
                        let after = if f[8] == "@prev" { Some(prev_next.clone()) } else { opt(f[8]) };
                        match engine.query_where(kind.as_deref(), owner.as_deref(), requester.as_deref(), pred.as_ref(), f[5], opt(f[6]).as_deref(), f[7] == "desc", after.as_deref(), f[9].parse().unwrap(), f[10].parse().unwrap()) {
                            Ok(page) => {
                                prev_next = page.next.clone();
                                format!("{} next={} examined={}", nodes_text(&page.nodes), page.next, page.examined)
                            }
                            Err(e) => format!("error: {e}"),
                        }
                    }
                    "count" => match engine.count_where(kind.as_deref(), owner.as_deref(), requester.as_deref(), pred.as_ref(), f[5]) {
                        Ok(n) => n.to_string(),
                        Err(e) => format!("error: {e}"),
                    },
                    "aggregate" => {
                        let spec = AggSpec::new(AggFunc::parse(f[13]).unwrap(), Some(f[14].to_string())).unwrap();
                        match engine.aggregate_where(kind.as_deref(), owner.as_deref(), requester.as_deref(), pred.as_ref(), f[5], &spec) {
                            Ok(v) => v.to_string(),
                            Err(e) => format!("error: {e}"),
                        }
                    }
                    "countBy" | "aggregateBy" => {
                        let values: Option<Vec<serde_json::Value>> = opt(f[12]).map(|j| serde_json::from_str(&j).expect("values json"));
                        let show = |v: &Option<serde_json::Value>| v.as_ref().map(|x| x.to_string()).unwrap_or_else(|| "none".to_string());
                        if f[0] == "countBy" {
                            match engine.count_by(kind.as_deref(), owner.as_deref(), requester.as_deref(), pred.as_ref(), f[5], f[11], values.as_deref()) {
                                Ok(gs) => format!("[{}]", gs.iter().map(|g| format!("{}={}", show(&g.value), g.count)).collect::<Vec<_>>().join(",")),
                                Err(e) => format!("error: {e}"),
                            }
                        } else {
                            let spec = AggSpec::new(AggFunc::parse(f[13]).unwrap(), Some(f[14].to_string())).unwrap();
                            match engine.aggregate_by(kind.as_deref(), owner.as_deref(), requester.as_deref(), pred.as_ref(), f[5], f[11], values.as_deref(), &spec) {
                                Ok(gs) => format!("[{}]", gs.iter().map(|g| format!("{}={}", show(&g.value), g.result)).collect::<Vec<_>>().join(",")),
                                Err(e) => format!("error: {e}"),
                            }
                        }
                    }
                    other => panic!("unknown query op {other}"),
                };
                println!("{res}");
            }
        }
        // fqserver.fct's counterpart: `facetql start`'s server path — the
        // environment's legacy aliases applied, Database::new over
        // FACETQL_DATA_DIR, create_router — served over plain HTTP on
        // 127.0.0.1:FACETQL_PORT until killed.
        "serve" => {
            facetql::config::apply_legacy_env_aliases();
            if let Some(dir) = std::env::var_os("FACETQL_DATA_DIR") {
                facetql::config::set_data_dir(dir.into());
            }
            let port: u16 = std::env::var("FACETQL_PORT").expect("FACETQL_PORT").parse().expect("port");
            let db = Arc::new(facetql::database::Database::new().expect("open database"));
            let app = facetql::api::routes::create_router(db);
            let rt = tokio::runtime::Runtime::new().expect("tokio runtime");
            // With FACETQL_TLS_IDENTITY, `facetql start --tls-identity`'s
            // path: the PKCS#12 identity through native-tls, served by
            // tls_server::serve_tls.
            let tls = std::env::var("FACETQL_TLS_IDENTITY").ok().map(|path| {
                let password = std::env::var("FACETQL_TLS_IDENTITY_PASSWORD").unwrap_or_default();
                let bytes = std::fs::read(&path).expect("read TLS identity");
                let identity = native_tls::Identity::from_pkcs12(&bytes, &password).expect("load TLS identity");
                let acceptor: tokio_native_tls::TlsAcceptor = native_tls::TlsAcceptor::new(identity).expect("TLS acceptor").into();
                acceptor
            });
            rt.block_on(async move {
                let listener = tokio::net::TcpListener::bind(("127.0.0.1", port)).await.expect("bind");
                println!("listening");
                match tls {
                    Some(acceptor) => {
                        facetql::tls_server::serve_tls(listener, app, acceptor, std::future::pending::<()>()).await
                    }
                    None => axum::serve(listener, app).await.expect("serve"),
                }
            });
        }
        // fqjson.fct's fjCanonText: each stdin line parsed by serde_json,
        // written back, and its encode_order_value bytes.
        "json-canon" => {
            for line in std::io::stdin().lock().lines() {
                let line = line.unwrap();
                match serde_json::from_str::<serde_json::Value>(&line) {
                    Ok(v) => println!("{} | {}", v, hex(&facetql::storage::index::encode_order_value(Some(&v)))),
                    Err(_) => println!("error"),
                }
            }
        }
        // One page script per stdin line (ops separated by ';').
        "page-script" => {
            for line in std::io::stdin().lock().lines() {
                println!("{}", page_script(&line.unwrap().replace(';', "\n")).replace('\n', "|"));
            }
        }
        // A stored page (nonce || ct || tag, hex) per stdin line.
        "open-page" => {
            for line in std::io::stdin().lock().lines() {
                let blob = unhex(line.unwrap().trim());
                match crypto::decrypt(&blob) {
                    Ok(body) => println!("{}", decode_text(&body)),
                    Err(e) => println!("error: {e}"),
                }
            }
        }
        // An encoded page body (hex) per stdin line, sealed as the pager
        // stores it.
        "seal-page" => {
            for line in std::io::stdin().lock().lines() {
                let body = unhex(line.unwrap().trim());
                assert_eq!(body.len(), PAGE_BODY_LEN);
                println!("{}", hex(&crypto::encrypt(&body)));
            }
        }
        // Read every page of a paged file through the real Pager.
        "pager-read" => {
            let pager = Pager::open(Path::new(&args[2])).expect("open");
            let mut out = format!("pages={};", pager.page_count());
            for id in 0..pager.page_count() {
                match pager.read(id) {
                    Ok(p) => out += &format!("read {id}={};", cells(&p)),
                    Err(e) => out += &format!("read {id} error: {e};"),
                }
            }
            println!("{out}");
        }
        // Write heap pages (`write ID HEXCELL`, ';'-separated) through the
        // real Pager, then flush.
        "pager-write" => {
            let pager = Pager::open(Path::new(&args[2])).expect("open");
            for op in args[3].split(';') {
                let w: Vec<&str> = op.split_whitespace().collect();
                if w.len() == 3 && w[0] == "write" {
                    let mut p = Page::new(PageKind::Heap);
                    p.push_cell(&unhex(w[2]));
                    pager.write(w[1].parse().unwrap(), p).expect("write");
                }
            }
            pager.flush().expect("flush");
            println!("pages={}", pager.page_count());
        }
        // A WAL frame line per stdin line, decoded by the real decoder.
        "wal-decode" => {
            for line in std::io::stdin().lock().lines() {
                match decode_frame(&line.unwrap()) {
                    FrameOutcome::Durable(r) => println!("durable {}", hex(&bincode::serialize(&r).unwrap())),
                    FrameOutcome::Torn(d) => println!("torn {d}"),
                    FrameOutcome::Corrupt(d) => println!("corrupt {d}"),
                }
            }
        }
        // A users log read back through read_all_records_framed, in
        // binary.fct's bnReadText format.
        "users-read" => {
            match binary::read_all_records_framed::<UserOpRecord>(Path::new(&args[2])) {
                Ok((records, tail)) => {
                    let mut out = String::new();
                    for (off, r) in records {
                        match r {
                            UserOpRecord::Put(u) => {
                                let role = if u.role == Role::Admin { "admin" } else { "user" };
                                out += &format!("{off}:put {} {} {role};", u.token_hash, u.owner);
                            }
                            UserOpRecord::Revoke(h) => out += &format!("{off}:revoke {h};"),
                        }
                    }
                    match tail {
                        TailState::Clean => out += "clean",
                        TailState::Truncated { offset, detail } => out += &format!("torn@{offset} {detail}"),
                    }
                    println!("{out}");
                }
                Err(e) => println!("error: {e}"),
            }
        }
        // Append `put HASH OWNER ROLE` / `revoke HASH` (';'-separated)
        // through append_record.
        "users-append" => {
            let mut out = String::new();
            for op in args[3].split(';') {
                let w: Vec<&str> = op.split_whitespace().collect();
                let rec = match w.as_slice() {
                    ["put", h, o, r] => UserOpRecord::Put(UserRecord {
                        token_hash: h.to_string(),
                        owner: o.to_string(),
                        role: if *r == "admin" { Role::Admin } else { Role::User },
                    }),
                    ["revoke", h] => UserOpRecord::Revoke(h.to_string()),
                    _ => continue,
                };
                let off = binary::append_record(Path::new(&args[2]), &rec).expect("append");
                out += &format!("@{off} ");
            }
            println!("{out}");
        }
        // A B+tree script (fqbtree.fct's btScriptText ops, ';'-separated)
        // over the index file at PATH; results, then entries=N and every
        // entry.
        "btree-script" => {
            let tree = BTree::open(Path::new(&args[2])).expect("open");
            let mut out = String::new();
            let hexarg = |w: &[&str], i: usize| -> Vec<u8> {
                match w.get(i) {
                    Some(&"-") | None => Vec::new(),
                    Some(h) => unhex(h),
                }
            };
            for op in args[3].split(';') {
                let w: Vec<&str> = op.split_whitespace().collect();
                if w.is_empty() {
                    continue;
                }
                match w[0] {
                    "put" => {
                        if let Err(e) = tree.put(&hexarg(&w, 1), &hexarg(&w, 2)) {
                            out += &format!("put error: {e};");
                        }
                    }
                    "del" => out += &format!("del {};", tree.remove(&hexarg(&w, 1)).expect("remove")),
                    "get" => match tree.get(&hexarg(&w, 1)).expect("get") {
                        Some(v) => out += &format!("get={};", hex(&v)),
                        None => out += "get none;",
                    },
                    "seek" => {
                        let mode = match w[1] {
                            "gt" => SeekMode::Gt,
                            "le" => SeekMode::Le,
                            "lt" => SeekMode::Lt,
                            _ => SeekMode::Ge,
                        };
                        match tree.seek(&unhex(w[2]), mode).expect("seek") {
                            Some((k, _)) => out += &format!("seek={};", hex(&k)),
                            None => out += "seek none;",
                        }
                    }
                    "scan" => {
                        let rev = w.get(2) == Some(&"rev");
                        let mut keys = Vec::new();
                        tree.for_each_range(&hexarg(&w, 1), None, rev, |k, _| {
                            keys.push(hex(k));
                            Ok(true)
                        })
                        .expect("scan");
                        out += &format!("scan={}", keys.len());
                        for k in keys {
                            out += &format!(" {k}");
                        }
                        out += ";";
                    }
                    "commit" => {
                        tree.commit().expect("commit");
                        out += "commit;";
                    }
                    other => panic!("unknown btree op {other}"),
                }
            }
            let mut all = Vec::new();
            tree.for_each_range(&[], None, false, |k, v| {
                all.push(format!("{}={}", hex(k), hex(v)));
                Ok(true)
            })
            .expect("scan");
            println!("{out}|entries={}|{}", tree.len(), all.join("|"));
        }
        // fqheap.fct's hpScriptText over the heap in $HOME/.facetql.
        "heap-script" => {
            let catalog = Arc::new(Catalog::open().expect("catalog"));
            let store = RecordStore::open(Arc::clone(&catalog));
            let mut out = String::new();
            let node = |addr: &str, n: usize| {
                let mut nd = Node::new(Coordinate::new(1, 2, 3, 250), addr.to_string(), "Post".to_string(), "alice".to_string());
                nd.value = 7;
                nd.data = "x".repeat(n);
                nd
            };
            let loc_text = |l: RecordLocation| format!("{}:{}:{}:{}", l.segment, l.page, l.slot, l.length);
            for op in args[2].split(';') {
                let w: Vec<&str> = op.split_whitespace().collect();
                if w.is_empty() {
                    continue;
                }
                match w[0] {
                    "node" => {
                        let l = store.append(&HeapRecord::Node(node(w[1], w[2].parse().unwrap()))).expect("append");
                        out += &format!("@{};", loc_text(l));
                    }
                    "history" => {
                        let e = HistoryEntry { address: w[1].to_string(), archived_at_unix: 1700000000, node: node(w[1], w[3].parse().unwrap()), version: w[2].parse().unwrap() };
                        let l = store.append(&HeapRecord::History(e)).expect("append");
                        out += &format!("@{};", loc_text(l));
                    }
                    "edge" => {
                        let e = Edge::new(w[1].to_string(), w[2].to_string(), w[3].to_string(), "alice".to_string());
                        let l = store.append(&HeapRecord::Edge(e)).expect("append");
                        out += &format!("@{};", loc_text(l));
                    }
                    "read" => {
                        let f: Vec<u32> = w[1].split(':').map(|x| x.parse().unwrap()).collect();
                        let l = RecordLocation { segment: f[0], page: f[1], slot: f[2] as u16, length: f[3] };
                        match store.read(l) {
                            Ok(HeapRecord::Node(n)) => out += &format!("node {} {} data={} value={};", n.address, n.kind, n.data.len(), n.value),
                            Ok(HeapRecord::Edge(e)) => out += &format!("edge {}>{}:{}@{};", e.from, e.to, e.kind, e.owner),
                            Ok(HeapRecord::History(h)) => out += &format!("history {} v{} t{} data={};", h.address, h.version, h.archived_at_unix, h.node.data.len()),
                            Err(e) => out += &format!("read error: {e};"),
                        }
                    }
                    "scan" => {
                        let mut recs = String::new();
                        store
                            .scan_segment(w[1].parse().unwrap(), |l, r| {
                                let t = match r {
                                    HeapRecord::Node(n) => format!("node {} {} data={} value={}", n.address, n.kind, n.data.len(), n.value),
                                    HeapRecord::Edge(e) => format!("edge {}>{}:{}@{}", e.from, e.to, e.kind, e.owner),
                                    HeapRecord::History(h) => format!("history {} v{} t{} data={}", h.address, h.version, h.archived_at_unix, h.node.data.len()),
                                };
                                recs += &format!("@{} {t};", loc_text(l));
                                Ok(())
                            })
                            .expect("scan");
                        out += &recs;
                        out += "scan;";
                    }
                    "drop" => {
                        store.drop_segment(w[1].parse().unwrap()).expect("drop");
                        out += "drop;";
                    }
                    "sync" => {
                        store.sync().expect("sync");
                        out += "sync;";
                    }
                    other => panic!("unknown heap op {other}"),
                }
            }
            println!("{out}|{}", catalog.with(catalog_text));
        }
        // The catalog in $HOME/.facetql, in catalog.fct's ctText format.
        "catalog-read" => match Catalog::open() {
            Ok(c) => println!("{}", c.with(catalog_text)),
            Err(e) => println!("error: {e}"),
        },
        // Open, apply `segs id:pages:obsolete,…` / `active N` / `next N`,
        // save.
        "catalog-write" => {
            let c = Catalog::open().expect("open");
            let parts: Vec<&str> = args[2].split_whitespace().collect();
            for kv in parts.chunks(2) {
                if kv.len() < 2 {
                    continue;
                }
                let (k, v) = (kv[0], kv[1]);
                c.update(|d| match k {
                    "active" => d.active_segment = v.parse().unwrap(),
                    "next" => d.next_segment = v.parse().unwrap(),
                    "segs" => {
                        d.segments = v
                            .split(',')
                            .map(|it| {
                                let f: Vec<u64> = it.split(':').map(|x| x.parse().unwrap()).collect();
                                SegmentMeta { id: f[0] as u32, pages: f[1] as u32, obsolete_bytes: f[2] }
                            })
                            .collect()
                    }
                    _ => {}
                });
            }
            c.save().expect("save");
            println!("{}", c.with(catalog_text));
        }
        // The checkpoint in $HOME/.facetql, read as recovery reads it.
        "ckpt-read" => match checkpoint::read() {
            Ok(v) => println!("value={v}"),
            Err(e) => println!("error: {e}"),
        },
        // `fence N`, `release N`, `advance N` (';'-separated), each advance
        // reporting the durable value after it and whether it moved.
        "ckpt-script" => {
            let mut out = String::new();
            for op in args[2].split(';') {
                let w: Vec<&str> = op.split_whitespace().collect();
                if w.len() != 2 {
                    continue;
                }
                let n: u64 = w[1].parse().unwrap();
                match w[0] {
                    "fence" => {
                        checkpoint::begin_fence(n);
                        out += &format!("fence {n};");
                    }
                    "release" => {
                        checkpoint::release_fence(n);
                        out += &format!("release {n};");
                    }
                    "advance" => {
                        let before = checkpoint::read().expect("read");
                        if let Err(e) = checkpoint::advance(n) {
                            out += &format!("error: {e}");
                            break;
                        }
                        let after = checkpoint::read().expect("read");
                        out += &format!("advance {n}={after}");
                        if after != before {
                            out += " written";
                        }
                        out += ";";
                    }
                    _ => {}
                }
            }
            println!("{out}");
        }
        _ => {
            eprintln!("usage: facetql_check page-script|open-page|seal-page|pager-read PATH|pager-write PATH OPS|wal-decode");
            std::process::exit(2);
        }
    }
}

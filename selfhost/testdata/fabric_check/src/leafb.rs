//! fabric_leaf_b_test.go's live checks: the fct side's wire output
//! decoded by the real fabric-facetql types.
//!
//! Modes (each `leafb-*`):
//! * `leafb-f64-text`: each stdin line is an f64 bit pattern as a signed
//!   decimal; prints `serde_json::to_string` of that f64.
//! * `leafb-path-<T>` (T = value | stats | message): each stdin line is a
//!   hex-encoded body (raw bytes) decoded as T through
//!   serde_path_to_error, then `Deserializer::end`; prints `ok` or
//!   `<category>|<path>|<serde_json error Display>` (category as
//!   Error::classify, path as serde_path_to_error::Error's Display shows
//!   it, empty when that omits it).
//! * `leafb-stats-decode`: each stdin line is a `GET /stats` body; prints
//!   `ok:<digest>` of `serde_json::from_str::<EngineStats>`, or
//!   `err:<serde_json error Display>`. The digest is the one
//!   fabric_stats.fct's statsDigestEngineStats prints (every field in
//!   wire.rs order, u64 as decimal, f64 as its bit pattern, None as `-`,
//!   strings as serde_json string literals).

use fabric_facetql::wire::{CellStats, EngineStats, KindCount, LatencyStats};
use std::io::BufRead;

fn fb(f: f64) -> String { (f.to_bits() as i64).to_string() }
fn q(s: &str) -> String { serde_json::to_string(s).unwrap() }
fn ou(o: Option<u64>) -> String { o.map(|v| v.to_string()).unwrap_or("-".into()) }
fn of(o: Option<f64>) -> String { o.map(fb).unwrap_or("-".into()) }
fn os(o: &Option<String>) -> String { o.as_ref().map(|s| q(s)).unwrap_or("-".into()) }
fn lat(l: &LatencyStats) -> String { format!("({},{},{},{})", l.count, ou(l.p50_us), ou(l.p99_us), ou(l.max_us)) }

pub fn digest(s: &EngineStats) -> String {
    let kinds: Vec<String> = s.kinds.iter().map(|k: &KindCount| format!("({},{})", q(&k.kind), k.count)).collect();
    let st = &s.storage;
    let rq = &s.runtime.requests;
    let w = &s.runtime.window;
    let p = &s.runtime.process;
    let c = &s.cells;
    let cells: Vec<String> = c.cells.iter().map(|x: &CellStats| format!("({},{},{},{},{},{},{},{})", x.x, x.y, x.z, x.q, x.reads, x.writes, x.bytes_read, x.bytes_written)).collect();
    format!("({},{},{},{},[{}],{},{},({},{},{},{}),{},({},({},{},{},{},{},{},{},{},{}),({},{},{},{},{}),({},{},{},{},{},{})),({},{},{},{},{},[{}]))",
        s.node_count, s.edge_count, s.user_count, s.history_entries, kinds.join(";"), s.reads_total, s.writes_total,
        st.page_size, st.segments, st.pages, st.obsolete_bytes, os(&s.version),
        s.runtime.uptime_seconds, rq.total, rq.read, rq.write, rq.excluded, rq.unclassified, rq.in_flight, rq.max_concurrent, rq.write_queue_depth, rq.write_queue_contended_total,
        w.duration_ms, w.age_ms, of(w.cpu_utilization), lat(&w.read_latency), lat(&w.write_latency),
        of(p.cpu_seconds_total), ou(p.cpu_cores), ou(p.resident_bytes), ou(p.memory_limit_bytes), os(&p.memory_limit_source), of(p.memory_utilization),
        c.capacity, c.tracked, c.overflow_reads, c.overflow_writes, c.unattributed_writes, cells.join(";"))
}

fn path_line<T: serde::de::DeserializeOwned>(body: &[u8]) -> String {
    let mut de = serde_json::Deserializer::from_slice(body);
    let r: Result<T, serde_path_to_error::Error<serde_json::Error>> = serde_path_to_error::deserialize(&mut de);
    let (path, e) = match r {
        Ok(_) => match de.end() {
            Ok(()) => return "ok".to_string(),
            Err(e) => (String::new(), e),
        },
        Err(e) => {
            let shown = e.to_string();
            let inner = e.inner().to_string();
            let path = shown.strip_suffix(&inner).and_then(|p| p.strip_suffix(": ")).unwrap_or("").to_string();
            (path, e.into_inner())
        }
    };
    let cat = match e.classify() {
        serde_json::error::Category::Io => "io",
        serde_json::error::Category::Syntax => "syntax",
        serde_json::error::Category::Data => "data",
        serde_json::error::Category::Eof => "eof",
    };
    format!("{cat}|{path}|{e}")
}

/// `leafb-plan`: frontdoor/plan.rs over one request per stdin line, in the
/// digest fabric_leafb/plan_check.fct prints (and plan_rust.tsv holds): the
/// line is hex of `keyspace US preference US method US path US query US
/// body` (query: `1<text>` present, `0` absent), over the check's six
/// keyspaces and four read preferences.
fn plan_line(hex: &str) -> String {
    use axum::http::Method;
    use fabric_core::Coordinate;
    use fabric_facetql::frontdoor::plan::plan;
    use fabric_facetql::frontdoor::{Agreement, Keyspace, KeyspaceRule, Plan};
    use fabric_routing::{ReadPreference, RouteIntent, RoutingKey};

    let bytes: Vec<u8> = (0..hex.len() / 2).map(|i| u8::from_str_radix(&hex[2 * i..2 * i + 2], 16).expect("hex")).collect();
    let mut parts: Vec<Vec<u8>> = vec![Vec::new()];
    for b in bytes {
        if b == 31 && parts.len() < 6 {
            parts.push(Vec::new());
        } else {
            parts.last_mut().unwrap().push(b);
        }
    }
    let text = |i: usize| String::from_utf8_lossy(&parts[i]).into_owned();
    let key = |shard: u64, x: u8, y: u8| RoutingKey::new(shard, Coordinate::new(x, y)).expect("an on-grid key");
    let rule = |ks: Keyspace, kind: &str, prefix: &str, k: RoutingKey| ks.with_rule(KeyspaceRule::new(kind, prefix, k).expect("a rule")).expect("a keyspace");
    let keyspace = match text(0).as_str() {
        "0" => rule(rule(Keyspace::new(), "Post", "Post:", key(1, 0, 0)), "User", "User:", key(2, 0, 0)),
        "1" => rule(rule(Keyspace::new(), "Post", "Post:", key(1, 0, 0)), "User", "User:", key(2, 3, 4)).with_fallback(key(9, 11, 12)),
        "2" => Keyspace::single(key(7, 0, 0)),
        "3" => rule(rule(rule(Keyspace::new(), "A", "__", key(1, 0, 0)), "B", "__fabric_placement:", key(2, 0, 0)), "C", "__fabric", key(3, 0, 0)),
        "4" => rule(Keyspace::new(), "Post", "Post:", key(1, 0, 0)).with_fallback(key(1, 0, 0)),
        _ => Keyspace::new(),
    };
    let preference = match text(1).as_str() {
        "1" => ReadPreference::PreferRegion { region: "eu".to_string() },
        "2" => ReadPreference::AnyCopy,
        "3" => ReadPreference::AnyFresh,
        _ => ReadPreference::Primary,
    };
    let method = Method::from_bytes(&parts[2]).expect("a method");
    let q = text(4);
    let query = q.strip_prefix('1');
    let body = parts[5].clone();
    match plan(&method, &text(3), query, &body, &keyspace, &preference) {
        Plan::Colocated { keys } => {
            let xs: Vec<String> = keys
                .iter()
                .map(|k| {
                    let intent = match &k.intent {
                        RouteIntent::Write => "W".to_string(),
                        RouteIntent::Read(ReadPreference::AnyFresh) => "RF".to_string(),
                        RouteIntent::Read(ReadPreference::PreferRegion { region }) => format!("RG({region})"),
                        RouteIntent::Read(ReadPreference::AnyCopy) => "RC".to_string(),
                        RouteIntent::Read(ReadPreference::Primary) => "RP".to_string(),
                    };
                    format!("{}~{}~{}", k.key, intent, k.origin)
                })
                .collect();
            format!("C|{}", xs.join(";"))
        }
        Plan::Broadcast { agreement, what } => format!(
            "B|{}|{}",
            if agreement == Agreement::Required { "Required" } else { "NotRequired" },
            what
        ),
        Plan::Subscribe => "S".to_string(),
        Plan::Refuse(refusal) => format!("R|{}|{}", refusal.status().as_u16(), refusal),
    }
}

pub fn run(mode: &str) {
    if mode == "leafb-plan" {
        for line in std::io::stdin().lock().lines() {
            println!("{}", plan_line(&line.expect("stdin")));
        }
        return;
    }
    if let Some(t) = mode.strip_prefix("leafb-path-") {
        for line in std::io::stdin().lock().lines() {
            let line = line.expect("stdin");
            let body: Vec<u8> = (0..line.len() / 2).map(|i| u8::from_str_radix(&line[2 * i..2 * i + 2], 16).expect("hex")).collect();
            let out = match t {
                "value" => path_line::<serde_json::Value>(&body),
                "stats" => path_line::<EngineStats>(&body),
                "message" => path_line::<fabric_protocol::FabricMessage>(&body),
                other => panic!("unknown path type {other}"),
            };
            println!("{out}");
        }
        return;
    }
    match mode {
        "leafb-f64-text" => {
            for line in std::io::stdin().lock().lines() {
                let bits: i64 = line.expect("stdin").trim().parse().expect("bits");
                println!("{}", serde_json::to_string(&f64::from_bits(bits as u64)).unwrap());
            }
        }
        "leafb-stats-decode" => {
            for line in std::io::stdin().lock().lines() {
                let line = line.expect("stdin");
                match serde_json::from_str::<EngineStats>(&line) {
                    Ok(s) => println!("ok:{}", digest(&s)),
                    Err(e) => println!("err:{e}"),
                }
            }
        }
        other => {
            eprintln!("fabric_check: unknown mode {other:?}");
            std::process::exit(2);
        }
    }
}

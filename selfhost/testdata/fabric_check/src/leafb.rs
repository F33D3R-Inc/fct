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

pub fn run(mode: &str) {
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

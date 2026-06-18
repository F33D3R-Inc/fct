//! Render-IR interpreter and neutral expression evaluator. Mirrors the JS/Python
//! runtimes: the flat op stream is parsed into a `Node` tree, and `eval` walks the
//! neutral irExpr JSON AST against a data scope. The child-facet case is handled
//! by App (app.rs), which owns the facet registry.

use crate::json::Json;
use std::collections::HashMap;

/// A parsed render node (the block-structured form of the flat IR op stream).
pub enum Node {
    Text(String),
    Expr(Json),
    If { x: Json, then_: Vec<Node>, els: Vec<Node> },
    For { var: String, x: Json, body: Vec<Node> },
    Child { name: String, props: Vec<Prop> },
}

pub struct Prop {
    pub name: String,
    pub x: Option<Json>, // value expression; None ⇒ literal
    pub lit: String,
}

/// build_tree parses the flat `render` op array into a Node tree.
pub fn build_tree(ops: &[Json]) -> Vec<Node> {
    let mut pos = 0;
    block(ops, &mut pos)
}

fn block(ops: &[Json], pos: &mut usize) -> Vec<Node> {
    let mut nodes = Vec::new();
    while *pos < ops.len() {
        let op = &ops[*pos];
        let kind = op.get("op").and_then(|j| j.as_str()).unwrap_or("");
        if kind == "end" || kind == "else" {
            return nodes;
        }
        *pos += 1;
        match kind {
            "text" => nodes.push(Node::Text(op.get("v").and_then(|j| j.as_str()).unwrap_or("").to_string())),
            "expr" => nodes.push(Node::Expr(op.get("x").cloned().unwrap_or(Json::Null))),
            "child" => {
                let mut props = Vec::new();
                if let Some(ps) = op.get("props").and_then(|j| j.as_arr()) {
                    for p in ps {
                        props.push(Prop {
                            name: p.get("name").and_then(|j| j.as_str()).unwrap_or("").to_string(),
                            x: p.get("x").cloned(),
                            lit: p.get("lit").and_then(|j| j.as_str()).unwrap_or("").to_string(),
                        });
                    }
                }
                nodes.push(Node::Child {
                    name: op.get("name").and_then(|j| j.as_str()).unwrap_or("").to_string(),
                    props,
                });
            }
            "if" => {
                let then_ = block(ops, pos);
                let mut els = Vec::new();
                if *pos < ops.len() && ops[*pos].get("op").and_then(|j| j.as_str()) == Some("else") {
                    *pos += 1;
                    els = block(ops, pos);
                }
                if *pos < ops.len() && ops[*pos].get("op").and_then(|j| j.as_str()) == Some("end") {
                    *pos += 1;
                }
                nodes.push(Node::If { x: op.get("x").cloned().unwrap_or(Json::Null), then_, els });
            }
            "for" => {
                let body = block(ops, pos);
                if *pos < ops.len() && ops[*pos].get("op").and_then(|j| j.as_str()) == Some("end") {
                    *pos += 1;
                }
                nodes.push(Node::For {
                    var: op.get("var").and_then(|j| j.as_str()).unwrap_or("").to_string(),
                    x: op.get("x").cloned().unwrap_or(Json::Null),
                    body,
                });
            }
            _ => {}
        }
    }
    nodes
}

/// Scope for evaluation: the facet data plus any loop-variable locals.
pub struct Scope<'a> {
    pub data: &'a Json,
    pub locals: &'a HashMap<String, Json>,
}

/// eval evaluates a neutral irExpr AST node against `scope`.
pub fn eval(x: &Json, scope: &Scope) -> Json {
    match x.get("k").and_then(|j| j.as_str()).unwrap_or("") {
        "num" => {
            let n = x.get("n").and_then(|j| j.as_str()).unwrap_or("0");
            Json::Num(n.parse::<f64>().unwrap_or(0.0))
        }
        "str" => Json::Str(x.get("s").and_then(|j| j.as_str()).unwrap_or("").to_string()),
        "bool" => Json::Bool(x.get("b").and_then(|j| j.as_bool()).unwrap_or(false)),
        "path" => eval_path(x, scope),
        "call" => Json::Null, // app-provided functions are not evaluable in the static runtime
        "unary" => {
            let inner = eval(x.get("x").unwrap_or(&Json::Null), scope);
            if x.get("op").and_then(|j| j.as_str()) == Some("!") {
                Json::Bool(!truthy(&inner))
            } else {
                Json::Num(-num(&inner))
            }
        }
        "bin" => {
            let l = eval(x.get("l").unwrap_or(&Json::Null), scope);
            let r = eval(x.get("r").unwrap_or(&Json::Null), scope);
            eval_bin(x.get("op").and_then(|j| j.as_str()).unwrap_or(""), &l, &r)
        }
        _ => Json::Null,
    }
}

fn eval_path(x: &Json, scope: &Scope) -> Json {
    let segs: Vec<&str> = match x.get("segs").and_then(|j| j.as_arr()) {
        Some(a) => a.iter().filter_map(|j| j.as_str()).collect(),
        None => return Json::Null,
    };
    if segs.is_empty() {
        return Json::Null;
    }
    let local = x.get("local").and_then(|j| j.as_bool()).unwrap_or(false);
    let mut cur: Json = if local {
        scope.locals.get(segs[0]).cloned().unwrap_or(Json::Null)
    } else {
        scope.data.get(segs[0]).cloned().unwrap_or(Json::Null)
    };
    for seg in &segs[1..] {
        cur = cur.get(seg).cloned().unwrap_or(Json::Null);
    }
    cur
}

fn eval_bin(op: &str, a: &Json, b: &Json) -> Json {
    match op {
        "==" => Json::Bool(json_eq(a, b)),
        "!=" => Json::Bool(!json_eq(a, b)),
        "<" => Json::Bool(num(a) < num(b)),
        "<=" => Json::Bool(num(a) <= num(b)),
        ">" => Json::Bool(num(a) > num(b)),
        ">=" => Json::Bool(num(a) >= num(b)),
        "&&" => Json::Bool(truthy(a) && truthy(b)),
        "||" => {
            if truthy(a) {
                a.clone()
            } else {
                b.clone()
            }
        }
        "+" => {
            if matches!(a, Json::Str(_)) || matches!(b, Json::Str(_)) {
                Json::Str(format!("{}{}", fmt(a), fmt(b)))
            } else {
                Json::Num(num(a) + num(b))
            }
        }
        "-" => Json::Num(num(a) - num(b)),
        "*" => Json::Num(num(a) * num(b)),
        "/" => Json::Num(num(a) / num(b)),
        "%" => Json::Num(num(a) % num(b)),
        _ => Json::Null,
    }
}

fn json_eq(a: &Json, b: &Json) -> bool {
    match (a, b) {
        (Json::Num(x), Json::Num(y)) => x == y,
        (Json::Str(x), Json::Str(y)) => x == y,
        (Json::Bool(x), Json::Bool(y)) => x == y,
        (Json::Null, Json::Null) => true,
        _ => false,
    }
}

pub fn truthy(v: &Json) -> bool {
    match v {
        Json::Bool(b) => *b,
        Json::Num(n) => *n != 0.0,
        Json::Str(s) => !s.is_empty(),
        Json::Arr(a) => !a.is_empty(),
        Json::Obj(_) => true,
        Json::Null => false,
    }
}

fn num(v: &Json) -> f64 {
    match v {
        Json::Num(n) => *n,
        Json::Bool(b) => {
            if *b {
                1.0
            } else {
                0.0
            }
        }
        Json::Str(s) => s.parse::<f64>().unwrap_or(0.0),
        _ => 0.0,
    }
}

/// fmt renders a value for interpolation (pre-escape).
pub fn fmt(v: &Json) -> String {
    match v {
        Json::Null => String::new(),
        Json::Bool(b) => (if *b { "true" } else { "false" }).to_string(),
        Json::Num(n) => {
            if n.fract() == 0.0 && n.is_finite() {
                format!("{}", *n as i64)
            } else {
                format!("{}", n)
            }
        }
        Json::Str(s) => s.clone(),
        other => other.to_string(),
    }
}

pub fn html_escape(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for c in s.chars() {
        match c {
            '&' => out.push_str("&amp;"),
            '<' => out.push_str("&lt;"),
            '>' => out.push_str("&gt;"),
            '"' => out.push_str("&quot;"),
            '\'' => out.push_str("&#39;"),
            c => out.push(c),
        }
    }
    out
}

/// resolve_facet_id substitutes {path} holes in a facet_id pattern with values
/// from `data` (e.g. "LikeButton:post:{post.id}").
pub fn resolve_facet_id(pattern: &str, data: &Json) -> String {
    let mut out = String::new();
    let chars: Vec<char> = pattern.chars().collect();
    let mut i = 0;
    while i < chars.len() {
        if chars[i] == '{' {
            let mut j = i + 1;
            let mut path = String::new();
            while j < chars.len() && chars[j] != '}' {
                path.push(chars[j]);
                j += 1;
            }
            let mut cur = data.clone();
            for seg in path.split('.') {
                cur = cur.get(seg).cloned().unwrap_or(Json::Null);
            }
            out.push_str(&fmt(&cur));
            i = j + 1;
        } else {
            out.push(chars[i]);
            i += 1;
        }
    }
    out
}

/// inject_facet_id adds data-facet-id to the first element of a rendered body.
pub fn inject_facet_id(html: &str, id: &str) -> String {
    let bytes: Vec<char> = html.chars().collect();
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] == '<' && i + 1 < bytes.len() && bytes[i + 1].is_ascii_alphabetic() {
            // advance past tag name
            let mut j = i + 1;
            while j < bytes.len() && (bytes[j].is_ascii_alphanumeric() || bytes[j] == '-') {
                j += 1;
            }
            let mut out: String = bytes[..j].iter().collect();
            out.push_str(&format!(" data-facet-id=\"{}\"", id));
            out.extend(&bytes[j..]);
            return out;
        }
        i += 1;
    }
    html.to_string()
}

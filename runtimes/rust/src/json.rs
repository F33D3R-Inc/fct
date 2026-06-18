//! A minimal JSON value + parser + serializer, dependency-free. Enough to read
//! the compiler's render.json / manifest.json, decode the /events POST body, and
//! emit event frames. Objects preserve insertion order (Vec of pairs); lookups
//! are linear, which is fine for the small documents the runtime handles.

use std::fmt::Write as _;

#[derive(Clone, Debug)]
pub enum Json {
    Null,
    Bool(bool),
    Num(f64),
    Str(String),
    Arr(Vec<Json>),
    Obj(Vec<(String, Json)>),
}

impl Json {
    pub fn get(&self, key: &str) -> Option<&Json> {
        if let Json::Obj(pairs) = self {
            for (k, v) in pairs {
                if k == key {
                    return Some(v);
                }
            }
        }
        None
    }

    pub fn as_str(&self) -> Option<&str> {
        match self {
            Json::Str(s) => Some(s),
            _ => None,
        }
    }

    pub fn as_f64(&self) -> Option<f64> {
        match self {
            Json::Num(n) => Some(*n),
            _ => None,
        }
    }

    pub fn as_bool(&self) -> Option<bool> {
        match self {
            Json::Bool(b) => Some(*b),
            _ => None,
        }
    }

    pub fn as_arr(&self) -> Option<&Vec<Json>> {
        match self {
            Json::Arr(a) => Some(a),
            _ => None,
        }
    }

    /// Object helper: build from owned pairs.
    pub fn obj(pairs: Vec<(&str, Json)>) -> Json {
        Json::Obj(pairs.into_iter().map(|(k, v)| (k.to_string(), v)).collect())
    }

    /// Serialize to a compact JSON string.
    pub fn to_string(&self) -> String {
        let mut s = String::new();
        self.write(&mut s);
        s
    }

    fn write(&self, out: &mut String) {
        match self {
            Json::Null => out.push_str("null"),
            Json::Bool(b) => out.push_str(if *b { "true" } else { "false" }),
            Json::Num(n) => {
                if n.fract() == 0.0 && n.is_finite() {
                    let _ = write!(out, "{}", *n as i64);
                } else {
                    let _ = write!(out, "{}", n);
                }
            }
            Json::Str(s) => write_json_string(s, out),
            Json::Arr(a) => {
                out.push('[');
                for (i, v) in a.iter().enumerate() {
                    if i > 0 {
                        out.push(',');
                    }
                    v.write(out);
                }
                out.push(']');
            }
            Json::Obj(pairs) => {
                out.push('{');
                for (i, (k, v)) in pairs.iter().enumerate() {
                    if i > 0 {
                        out.push(',');
                    }
                    write_json_string(k, out);
                    out.push(':');
                    v.write(out);
                }
                out.push('}');
            }
        }
    }
}

fn write_json_string(s: &str, out: &mut String) {
    out.push('"');
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            c if (c as u32) < 0x20 => {
                let _ = write!(out, "\\u{:04x}", c as u32);
            }
            c => out.push(c),
        }
    }
    out.push('"');
}

/// parse decodes a JSON document, returning None on malformed input.
pub fn parse(s: &str) -> Option<Json> {
    let bytes: Vec<char> = s.chars().collect();
    let mut p = Parser { b: bytes, i: 0 };
    p.ws();
    let v = p.value()?;
    p.ws();
    Some(v)
}

struct Parser {
    b: Vec<char>,
    i: usize,
}

impl Parser {
    fn ws(&mut self) {
        while self.i < self.b.len() && self.b[self.i].is_whitespace() {
            self.i += 1;
        }
    }

    fn peek(&self) -> Option<char> {
        self.b.get(self.i).copied()
    }

    fn value(&mut self) -> Option<Json> {
        self.ws();
        match self.peek()? {
            '{' => self.object(),
            '[' => self.array(),
            '"' => Some(Json::Str(self.string()?)),
            't' => self.lit("true", Json::Bool(true)),
            'f' => self.lit("false", Json::Bool(false)),
            'n' => self.lit("null", Json::Null),
            _ => self.number(),
        }
    }

    fn lit(&mut self, word: &str, v: Json) -> Option<Json> {
        for c in word.chars() {
            if self.peek()? != c {
                return None;
            }
            self.i += 1;
        }
        Some(v)
    }

    fn object(&mut self) -> Option<Json> {
        self.i += 1; // {
        let mut pairs = Vec::new();
        self.ws();
        if self.peek()? == '}' {
            self.i += 1;
            return Some(Json::Obj(pairs));
        }
        loop {
            self.ws();
            let key = self.string()?;
            self.ws();
            if self.peek()? != ':' {
                return None;
            }
            self.i += 1;
            let val = self.value()?;
            pairs.push((key, val));
            self.ws();
            match self.peek()? {
                ',' => {
                    self.i += 1;
                }
                '}' => {
                    self.i += 1;
                    return Some(Json::Obj(pairs));
                }
                _ => return None,
            }
        }
    }

    fn array(&mut self) -> Option<Json> {
        self.i += 1; // [
        let mut items = Vec::new();
        self.ws();
        if self.peek()? == ']' {
            self.i += 1;
            return Some(Json::Arr(items));
        }
        loop {
            let v = self.value()?;
            items.push(v);
            self.ws();
            match self.peek()? {
                ',' => {
                    self.i += 1;
                }
                ']' => {
                    self.i += 1;
                    return Some(Json::Arr(items));
                }
                _ => return None,
            }
        }
    }

    fn string(&mut self) -> Option<String> {
        if self.peek()? != '"' {
            return None;
        }
        self.i += 1;
        let mut s = String::new();
        loop {
            let c = self.peek()?;
            self.i += 1;
            match c {
                '"' => return Some(s),
                '\\' => {
                    let e = self.peek()?;
                    self.i += 1;
                    match e {
                        '"' => s.push('"'),
                        '\\' => s.push('\\'),
                        '/' => s.push('/'),
                        'n' => s.push('\n'),
                        'r' => s.push('\r'),
                        't' => s.push('\t'),
                        'b' => s.push('\u{0008}'),
                        'f' => s.push('\u{000c}'),
                        'u' => {
                            let mut code = 0u32;
                            for _ in 0..4 {
                                let h = self.peek()?;
                                self.i += 1;
                                code = code * 16 + h.to_digit(16)?;
                            }
                            s.push(char::from_u32(code).unwrap_or('\u{fffd}'));
                        }
                        _ => return None,
                    }
                }
                c => s.push(c),
            }
        }
    }

    fn number(&mut self) -> Option<Json> {
        let start = self.i;
        while let Some(c) = self.peek() {
            if c.is_ascii_digit() || c == '-' || c == '+' || c == '.' || c == 'e' || c == 'E' {
                self.i += 1;
            } else {
                break;
            }
        }
        let text: String = self.b[start..self.i].iter().collect();
        text.parse::<f64>().ok().map(Json::Num)
    }
}

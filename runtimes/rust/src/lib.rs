//! Facet Architecture — Rust server runtime.
//!
//! The Rust analogue of the Go `fa` package: it turns the compiler's neutral
//! output (manifest.json + render.json, from `fct build` with target = "rust")
//! into a live app — SSE transport, HMAC-signed event push, the render-IR
//! interpreter, and the /events router. The wire format, signing layout, and
//! client runtime are shared with every other target (see docs/BACKENDS.md).
//!
//! Dependency-free: only the standard library. SHA-256/HMAC, a JSON parser, and a
//! minimal HTTP/1.1 + SSE server are all implemented here.

pub mod app;
pub mod framework;
pub mod json;
pub mod native;
pub mod render;
pub mod sha256;

pub use app::{App, Ctx};
pub use json::Json;

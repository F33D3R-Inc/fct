// Demo app on the Rust runtime. Build the IR first:
//
//   fct build ../../../cmd/fct/scaffold/facets/like_button.fct generated
//
// (with an fct.toml whose [compiler] target = "rust"), then:
//
//   cargo run --bin demo   # open http://localhost:7373 and click the heart

use fa_runtime::{App, Ctx, Json};
use std::sync::{Arc, Mutex};

struct State {
    count: i64,
    liked: bool,
}

fn data(s: &State) -> Json {
    Json::obj(vec![
        ("post", Json::obj(vec![("id", Json::Str("p1".into()))])),
        ("count", Json::Num(s.count as f64)),
        ("liked", Json::Bool(s.liked)),
    ])
}

fn main() {
    let state = Arc::new(Mutex::new(State { count: 3, liked: false }));
    let s_root = Arc::clone(&state);
    let s_like = Arc::clone(&state);

    let gen = format!("{}/generated", env!("CARGO_MANIFEST_DIR"));
    let runtime_js = format!("{}/../../runtime/fa-runtime.js", env!("CARGO_MANIFEST_DIR"));
    let key = std::env::var("FA_SIGNING_KEY").unwrap_or_default();

    let app = App::new(&gen, &key, "LikeButton - Rust runtime", &runtime_js)
        .root("LikeButton", move || data(&s_root.lock().unwrap()))
        .on("post.like", move |_ctx: &Ctx| {
            let mut s = s_like.lock().unwrap();
            s.liked = !s.liked;
            s.count += if s.liked { 1 } else { -1 };
            Some(data(&s))
        });

    app.listen("localhost:7373");
}

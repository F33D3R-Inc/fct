"""Demo app on the Python runtime. Build the IR first:

    fct build ../../../cmd/fct/scaffold/facets/like_button.fct generated

(with an fct.toml whose [compiler] target = "python"), then:

    python3 demo/server.py   # open http://localhost:7373 and click the heart
"""

import os
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from facet import App  # noqa: E402

state = {"post": {"id": "p1"}, "count": 3, "liked": False}


def current():
    return {"post": state["post"], "count": state["count"], "liked": state["liked"]}


def like(_ctx):
    state["liked"] = not state["liked"]
    state["count"] += 1 if state["liked"] else -1
    return current()


app = App(
    gen_dir=os.path.join(os.path.dirname(os.path.abspath(__file__)), "generated"),
    fa_key=os.environ.get("FA_SIGNING_KEY", ""),
    title="LikeButton - Python runtime",
)
app.root("LikeButton", lambda _ctx: current())
app.on("post.like", like)

if __name__ == "__main__":
    app.listen("localhost:7373")

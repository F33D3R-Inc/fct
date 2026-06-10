// Command demo is the end-to-end proof on the real framework. It compiles a .fct
// via fa.Compile and serves it inside the Playground — no hand-written shell,
// SSE, routing, or signing. It showcases:
//
//   - composition: ProfileHeader composes Avatar + FollowButton (<Child/> tags);
//
//   - scoped delivery: clicking Follow re-renders ONLY the FollowButton sub-facet
//     and the update goes ONLY to the clicking connection (no cross-client leak).
//
//     go run ./examples/demo            # http://localhost:7373
//     FA_ADDR=:9000 go run ./examples/demo path.fct
package main

import (
	"html/template"
	"log"
	"net/http"
	"os"

	"fct.dev/fa"
)

func main() {
	fctPath := "examples/composition.fct"
	if len(os.Args) > 1 {
		fctPath = os.Args[1]
	}
	src, err := os.ReadFile(fctPath)
	if err != nil {
		log.Fatalf("read %s: %v (run from the repo root)", fctPath, err)
	}
	c, err := fa.Compile(string(src))
	if err != nil {
		log.Fatalf("compile: %v", err)
	}

	// Server-authoritative state.
	user := map[string]any{"ID": "42", "Name": "Ada Lovelace", "Handle": "ada", "URL": "https://i.pravatar.cc/96?img=5"}
	following := false

	profile := func() template.HTML {
		return c.MustRender("ProfileHeader", map[string]any{"User": user, "Following": following})
	}
	followBtn := func() template.HTML {
		return c.MustRender("FollowButton", map[string]any{"Target": user, "Following": following})
	}

	app := fa.New(c.Manifest)
	app.On("follow.toggle", func(ctx fa.Ctx) ([]fa.Event, error) {
		following = !following
		// Surgical: re-render only the FollowButton child. Returned events go to
		// the acting connection only — the avatar and name are untouched, and no
		// other client sees this.
		return []fa.Event{{
			Op:       "replace",
			FacetID:  "FollowButton:target:" + user["ID"].(string),
			Fragment: string(followBtn()),
		}}, nil
	})

	mux := http.NewServeMux()
	app.Mount(mux) // /sse, /events, /manifest.json, /fa-runtime.js
	app.HandlePage(mux, fa.ShellOptions{Title: "Facet Architecture — composition", Theme: "dark", CSS: demoCSS},
		func(r *http.Request) template.HTML {
			return `<div class="card">` + profile() +
				`<p class="hint">click Follow — only the button re-renders, and only on your screen</p></div>`
		})

	addr := os.Getenv("FA_ADDR")
	if addr == "" {
		addr = "localhost:7373"
	}
	log.Printf("FA demo on http://%s  (compiled %s)", addr, fctPath)
	log.Fatal(http.ListenAndServe(addr, mux))
}

const demoCSS template.CSS = `
  body { font-family: system-ui, sans-serif; background:#15202b; color:#e7e9ea;
         display:grid; place-items:center; min-height:100vh; margin:0; }
  .card { text-align:center; }
  .profile-header { display:flex; align-items:center; gap:14px; justify-content:center; }
  .avatar { border-radius:999px; }
  .avatar--large { width:72px; height:72px; }
  .profile-header__meta { text-align:left; }
  .display-name { display:block; font-weight:700; font-size:18px; }
  .handle { color:#8b98a5; font-size:14px; }
  .btn-follow { background:#eff3f4; color:#0f1419; border:0; border-radius:999px;
                padding:8px 18px; font-weight:700; cursor:pointer; }
  .btn-follow--on { background:transparent; color:#e7e9ea; border:1px solid #536471; }
  .hint { color:#5c6e7e; font-size:13px; margin-top:18px; }
`

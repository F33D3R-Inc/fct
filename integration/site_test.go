package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"facet/internal/compile"
)

// facets/f33d3r_com.fct is f33d3r.com at one origin: the web pages and the
// /api/v2 surface over one entity model and one session store. An account
// made by the web's form is the account the native API answers for, one made
// through POST /api/v2/accounts signs in on the web, a post made on the web is
// a Work the API returns, and every page answers.
func TestSiteIsOneApp(t *testing.T) {
	e := startEngine(t)
	a := startFacetsApp(t, e, "f33d3r_com.fct")

	// The web: sign up through the page's form action, on a cookie session.
	if code, body := a.action("webSignup", "ada", "correct-horse"); code != 200 {
		t.Fatalf("webSignup: %d %s", code, body)
	}
	if code, body := a.get("/"); code != 200 || !strings.Contains(body, "signed in as @ada") {
		t.Fatalf("home after the web signup: %d, signed in: %v", code, strings.Contains(body, "signed in as @ada"))
	}
	if code, body := a.action("post", "hello from the web #garden"); code != 200 {
		t.Fatalf("post: %d %s", code, body)
	}
	// The same cookie session is the API's: /api/v2/me is ada.
	if code, body := a.get("/api/v2/me"); code != 200 || !strings.Contains(body, `"handle":"ada"`) {
		t.Fatalf("GET /api/v2/me on the web session: %d %.300s", code, body)
	}

	// The native API: sign up with a bearer token, then read ada's post.
	a.newSession()
	resp, err := http.Post(a.url("/api/v2/accounts"), "application/json", strings.NewReader(`{"handle":"bob","password":"battery-staple","terms_accepted":true}`))
	if err != nil {
		t.Fatal(err)
	}
	var session struct {
		Token string `json:"token"`
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	json.Unmarshal(raw, &session)
	if resp.StatusCode != 201 || session.Token == "" {
		t.Fatalf("POST /api/v2/accounts: %d %s", resp.StatusCode, raw)
	}
	bearer := func(path string) (int, string) {
		req, _ := http.NewRequest("GET", a.url(path), nil)
		req.Header.Set("Authorization", "Bearer "+session.Token)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b)
	}
	if code, body := bearer("/api/v2/me"); code != 200 || !strings.Contains(body, `"handle":"bob"`) {
		t.Fatalf("GET /api/v2/me as bob: %d %.300s", code, body)
	}
	if code, body := bearer("/api/v2/users/ada/works"); code != 200 || !strings.Contains(body, "hello from the web") {
		// the profile's works, as the native client reads them
		if code2, body2 := bearer("/api/v2/feed"); code2 != 200 || !strings.Contains(body2, "hello from the web") {
			t.Fatalf("ada's web post is not in the API: %d %.200s / %d %.300s", code, body, code2, body2)
		}
	}
	// bob, made by the API, signs in on the web.
	if code, body := a.action("webLogin", "bob", "battery-staple"); code != 200 {
		t.Fatalf("webLogin bob: %d %s", code, body)
	}
	if _, body := a.get("/"); !strings.Contains(body, "signed in as @bob") {
		t.Fatal("the web session after webLogin is not bob")
	}
	if code, body := a.action("like", 1); code != 200 {
		t.Fatalf("like: %d %s", code, body)
	}
	if code, body := a.action("webMessage", "ada", "hi ada"); code != 200 {
		t.Fatalf("webMessage: %d %s", code, body)
	}
	if code, body := a.action("post", "watching $AAPL today"); code != 200 {
		t.Fatalf("post: %d %s", code, body)
	}
	if code, body := a.action("webArticle", "Hello World", "", "the article body"); code != 200 {
		t.Fatalf("webArticle: %d %s", code, body)
	}
	if code, body := a.action("webGoLive", "late show"); code != 200 {
		t.Fatalf("webGoLive: %d %s", code, body)
	}

	// Every page answers, with the data it shows.
	for path, want := range map[string]string{
		"/": "hello from the web", "/profile/ada": "hello from the web", "/post/1": "hello from the web",
		"/tag/garden": "hello from the web", "/search": "Search", "/notifications": "Notifications",
		"/bookmarks": "Bookmarks", "/messages": "@ada", "/dm/1": "hi ada", "/live": "late show",
		"/browse/general": "", "/live/1": "late show", "/studio": "You're live", "/wallet": "Wallet",
		"/people": "@ada", "/login": "Sign in",
		// the reference's other pages (feed-engine internal/web)
		"/explore": "Trending", "/terms": "Governing law", "/privacy": "What we collect", "/dmca": "dmca@f33d3r.com",
		"/legal/2257": "18 years of age", "/deactivated": "deactivated", "/achievements": "Realm", "/analytics": "Top posts",
		"/articles": "Hello World", "/articles/new": "New article", "/blocked": "Blocked accounts", "/muted": "Muted words",
		"/create": "Creator studio", "/create/content": "Studio", "/create/welcome": "Become a creator", "/create/setup": "Creator setup",
		"/forgot-password": "Reset your password", "/kyc": "Identity verification", "/kyc/creator/statement": "Creator statement",
		"/library": "Library", "/lists": "Lists", "/marketplace": "Marketplace", "/music": "Music", "/onboard": "Welcome to F33D3R",
		"/settings": "Settings", "/settings/profile": "Edit profile", "/settings/security": "Change password",
		"/settings/notifications": "Likes", "/backup-codes/setup": "Backup codes", "/stocks/aapl": "watching $AAPL today",
		"/visions": "Visions", "/communities": "Communities", "/org/panel": "Organization", "/wallet/history": "Wallet",
		"/admin-console": "Operators only", "/work/1": "hello from the web",
		"/shop/ada": "shop",
	} {
		code, body := a.get(path)
		if code != 200 || !strings.Contains(body, want) {
			t.Errorf("GET %s = %d, contains %q: %v", path, code, want, strings.Contains(body, want))
		}
	}
	// The reader, at the slug the product minted.
	_, list := a.get("/articles")
	if m := regexp.MustCompile(`/article/([a-z0-9-]+)`).FindStringSubmatch(list); m == nil {
		t.Error("the articles page links no article")
	} else if code, body := a.get("/article/" + m[1]); code != 200 || !strings.Contains(body, "the article body") {
		t.Errorf("GET /article/%s = %d", m[1], code)
	}
	// A stock page lists the posts that name its ticker, or says there are
	// none yet; without a quote source it says quotes are unavailable.
	if _, body := a.get("/stocks/tsla"); !strings.Contains(body, "No posts about $TSLA yet") || !strings.Contains(body, "Quotes aren") {
		t.Error("an unmentioned ticker's page should say no posts and no quotes")
	}
	// Profiles live at the clean root URL, with their followers, following
	// and works beneath it; the literal pages still win their paths.
	for path, want := range map[string]string{
		"/ada": "hello from the web", "/ada/followers": "Followers", "/ada/following": "Following",
		"/ada/work/1": "hello from the web", "/bob/work/1": "doesn't exist", "/nobody-here": "doesn't exist",
		"/search": "Search", "/settings/followers": "Settings",
	} {
		if code, body := a.get(path); code != 200 || !strings.Contains(body, want) {
			t.Errorf("GET %s = %d, want %q", path, code, want)
		}
	}
	// Every page the site serves at its first segment is a handle no one can
	// take: a person named "live" would never reach their own /live.
	g, err := compile.File("../../facets/f33d3r_com.fct")
	if err != nil {
		t.Fatal(err)
	}
	a.newSession()
	seen := map[string]bool{}
	for _, pg := range g.Pages {
		seg := strings.SplitN(strings.Trim(pg.Path, "/"), "/", 2)[0]
		if seg == "" || strings.HasPrefix(seg, ":") || seen[seg] {
			continue
		}
		seen[seg] = true
		if code, body := a.action("signup", seg, "password1", seg, "1990-01-01", "safe", "", "", "phone", true); code == 200 || !strings.Contains(body, "reserved") {
			t.Errorf("signup as %q, a page of the site: %d %.120s", seg, code, body)
		}
	}
	a.newSession()
	if code, body := a.action("webLogin", "bob", "battery-staple"); code != 200 {
		t.Fatalf("webLogin bob again: %d %s", code, body)
	}
	// The profile says when the account was made the way the reference does.
	if _, body := a.get("/profile/ada"); !strings.Contains(body, "Joined "+time.Now().UTC().Format("January 2006")) {
		t.Error("the profile's joined line should read like \"Joined September 2026\"")
	}
	// The account page links the export the API serves, and the signed-in web
	// session can follow it.
	if _, body := a.get("/settings/account"); !strings.Contains(body, `href="/api/v2/me/export"`) {
		t.Error("the account settings page does not link the data export")
	}
	if code, body := a.get("/api/v2/me/export"); code != 200 || !strings.Contains(body, "bob") {
		t.Errorf("GET /api/v2/me/export as the web session = %d %.200s", code, body)
	}
	// Explore ranks by engagement, not age: bob's newer post has one like,
	// ada's older one a like and a repost (worth two).
	code, body := a.action("post", "quiet one")
	quiet := regexp.MustCompile(`"work_id":"(\d+)"`).FindStringSubmatch(body)
	if code != 200 || quiet == nil {
		t.Fatalf("post: %d %s", code, body)
	}
	for _, step := range [][]any{{"like", quiet[1]}, {"repost", 1}} {
		if code, body := a.action(step[0].(string), step[1:]...); code != 200 {
			t.Fatalf("%v: %d %s", step, code, body)
		}
	}
	if _, body := a.get("/explore"); !(strings.Contains(body, "quiet one") && strings.Index(body, "hello from the web") < strings.Index(body, "quiet one")) {
		t.Error("Explore should rank the more engaged older post above the newer one")
	}
	if code, body := a.get("/api/v2/contract"); code != 200 || !strings.Contains(body, "F33D3R native API") {
		t.Errorf("GET /api/v2/contract = %d", code)
	}
}

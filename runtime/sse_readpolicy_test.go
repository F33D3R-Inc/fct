package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"facet/internal/compile"
)

// sseReadPolicyApp declares Post with a `read:` policy — an author sees their
// own draft, a stranger only sees a published one — AND a typeahead sourced
// from Post. The typeahead is what puts Post in streamEntities (region.go): a
// bare collection reference the CLIENT's own evaluator resolves straight out of
// `store["Post"]`, with no request involved and so no place a name-only
// announcement could land a re-fetch (see fanout's own doc, and
// collectBareRefs/collectReads). Before this fix, that is exactly the case
// whose rows crossed the live stream to every subscriber whole, `read:` or not:
// an entity's own list view and the JSON API already folded `read:` into their
// query (applyReadPolicy/withEntityReadPolicy/apiQueryRows), but a subscriber
// channel had no actor at all to check it against.
const sseReadPolicyApp = `app Vault:
    state q: text = "" @client
    entity Post:
        id: int
        author: text
        title: text
        published: bool
        read: published || author == actor
    action write(title: text):
        add Post { author: actor, title: title, published: false }
    view Home at "/":
        box:
            typeahead bind q from Post.title placeholder "find"
`

// sseFrames opens /live as c and decodes its "data: " lines onto a channel. The
// returned cancel closes the request context, which is what unblocks
// handleLive's own `<-r.Context().Done()` and lets it drop the subscription.
func sseFrames(t *testing.T, c *http.Client, url string) (<-chan map[string]any, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	frames := make(chan map[string]any, 16)
	go func() {
		defer res.Body.Close()
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m); err == nil {
				select {
				case frames <- m:
				default:
				}
			}
		}
	}()
	return frames, cancel
}

// nextFrame waits for the next decoded SSE frame, failing the test rather than
// hanging forever if the stream never sends one.
func nextFrame(t *testing.T, frames <-chan map[string]any, timeout time.Duration) map[string]any {
	t.Helper()
	select {
	case f := <-frames:
		return f
	case <-time.After(timeout):
		t.Fatal("timed out waiting for an SSE frame")
		return nil
	}
}

// framePostTitles returns the `title` field of every Post row a frame's deltas
// carried (nil/empty when the frame carried none), so a test can assert on the
// data actually delivered rather than merely on whether the key is present —
// this feature ships an empty list for an excluded actor, not an absent key.
func framePostTitles(frame map[string]any) []string {
	deltas, _ := frame["deltas"].(map[string]any)
	rows, _ := deltas["Post"].([]any)
	var out []string
	for _, r := range rows {
		if rec, ok := r.(map[string]any); ok {
			if title, ok := rec["title"].(string); ok {
				out = append(out, title)
			}
		}
	}
	return out
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// TestLiveStreamFiltersReadPolicyGatedRows proves the fix: an entity that both
// streams as rows (a typeahead source, see sseReadPolicyApp) and carries a
// `read:` clause must not hand every subscriber the same rows regardless of who
// they are. Two actors connect to /live; alice writes a draft only she may
// read; her own stream must carry it live, and bob's stream — which does
// receive a frame for the same broadcast, since Post is still row-streamed —
// must not carry the row `read:` excludes him from.
func TestLiveStreamFiltersReadPolicyGatedRows(t *testing.T) {
	g, err := compile.String(sseReadPolicyApp)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	alice := jarClient(t)
	_, aliceCSRF := getPage(t, alice, ts.URL+"/?as=alice")
	bob := jarClient(t)
	getPage(t, bob, ts.URL+"/?as=bob")

	aliceFrames, aliceCancel := sseFrames(t, alice, ts.URL+"/live")
	defer aliceCancel()
	bobFrames, bobCancel := sseFrames(t, bob, ts.URL+"/live")
	defer bobCancel()

	// Drain each connection's opening "hello" snapshot (also read:-filtered by
	// this fix) before triggering the write, so the frame asserted on below is
	// unambiguously the fanout from that one action.
	nextFrame(t, aliceFrames, 2*time.Second)
	nextFrame(t, bobFrames, 2*time.Second)

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/event",
		strings.NewReader(`{"action":"write","args":["Nuclear codes"]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Facet-CSRF", aliceCSRF)
	res, err := alice.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("write action: status %d", res.StatusCode)
	}

	if titles := framePostTitles(nextFrame(t, aliceFrames, 2*time.Second)); !containsStr(titles, "Nuclear codes") {
		t.Errorf("the draft's own author must see it over the live stream, got Post titles %v", titles)
	}
	if titles := framePostTitles(nextFrame(t, bobFrames, 2*time.Second)); containsStr(titles, "Nuclear codes") {
		t.Errorf("read: did not protect the live stream from a second signed-in actor: got Post titles %v", titles)
	}
}

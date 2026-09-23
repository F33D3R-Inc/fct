package selfhost

// fabric_daemon_*.fct port fabric-daemon (fabricd). fabric_daemon_cases.fct
// answers one line per input line in exactly the text testdata/fabric_check
// prints from the real crate in its `daemon-*` modes (src/daemon.rs), so
// every comparison here is port against Rust:
//
//   - golden files (testdata/fabric_daemon_*.golden) hold what the real
//     crate printed for each corpus, so the port is checked with or without
//     cargo; and
//   - the *MatchesRust tests re-derive those lines from the live crate
//     whenever cargo is available, so a golden file cannot drift.
//
// Each Rust #[test] of config.rs / status.rs / liveness.rs / admin.rs /
// mover.rs is also replayed input for input with its own assertions against
// the port's output.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"facet/internal/compile"
	"facet/runtime"
)

var (
	fdtOnce sync.Once
	fdtSrv  *httptest.Server
	fdtErr  error
)

func fdtServer(t *testing.T) *httptest.Server {
	t.Helper()
	fdtOnce.Do(func() {
		g, err := compile.File("fabric_daemon_cases.fct")
		if err != nil {
			fdtErr = fmt.Errorf("compile selfhost/fabric_daemon_cases.fct: %v", err)
			return
		}
		srv, err := runtime.NewInMemory(g)
		if err != nil {
			fdtErr = err
			return
		}
		fdtSrv = httptest.NewServer(srv.Handler())
	})
	if fdtErr != nil {
		t.Fatal(fdtErr)
	}
	return fdtSrv
}

// fdtRun answers lines through the port's mode.
func fdtRun(t *testing.T, mode string, lines []string) []string {
	t.Helper()
	ts := fdtServer(t)
	d := postJSON(t, ts, "runDaemonCases", mode, strings.Join(lines, "\n"))
	out, _ := d["daemonCaseOut"].(string)
	if len(lines) == 0 {
		return nil
	}
	return strings.Split(out, "\n")
}

func fdtGolden(t *testing.T, name string) []string {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func fdtCompare(t *testing.T, what string, inputs, port, want []string) {
	t.Helper()
	if len(port) != len(inputs) || len(want) != len(inputs) {
		t.Fatalf("%s: %d inputs, port answered %d, expected %d", what, len(inputs), len(port), len(want))
	}
	bad := 0
	for i := range inputs {
		if port[i] != want[i] {
			bad++
			if bad <= 8 {
				t.Errorf("%s #%d %s:\n port %s\n rust %s", what, i, inputs[i], port[i], want[i])
			}
		}
	}
	if bad > 8 {
		t.Errorf("%s: %d mismatches in all", what, bad)
	}
}

// fdtLiveGolden runs the real crate over inputs and compares it with the
// golden file; with FDT_UPDATE_GOLDEN=1 it rewrites the golden file from
// the crate's output instead.
func fdtLiveGolden(t *testing.T, mode string, inputs []string, name string) {
	t.Helper()
	live := laRust(t, mode, inputs)
	if os.Getenv("FDT_UPDATE_GOLDEN") == "1" {
		if len(live) != len(inputs) {
			t.Fatalf("%s: %d inputs, rust answered %d", mode, len(inputs), len(live))
		}
		if err := os.WriteFile("testdata/"+name, []byte(strings.Join(live, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	golden := fdtGolden(t, name)
	fdtCompare(t, mode+" golden", inputs, golden, live)
}

// ---------------------------------------------------------------- config

var fdtAdminEnv = [][2]string{{"FABRIC_ADMIN_TOKEN", "admin-secret"}}

func fdtConfigLine(text string, env [][2]string) string {
	pairs := make([][]string, 0, len(env))
	for _, p := range env {
		pairs = append(pairs, []string{p[0], p[1]})
	}
	b, err := json.Marshal(map[string]any{"text": text, "env": pairs})
	if err != nil {
		panic(err)
	}
	return string(b)
}

const fdtMinimal = `{
        "backends": [
            {
                "id": "db-a",
                "url": "http://db-a:8892",
                "region": "us-east",
                "placements": [{ "shard": 1, "x": 0, "y": 0 }]
            }
        ],
        "keyspace": { "fallback": { "shard": 1, "x": 0, "y": 0 } }
    }`

// fdtWith is the one-backend declaration with `extra` spliced in as more
// top-level members.
func fdtWith(extra string) string {
	base := `{"backends": [{"id": "db-a", "url": "http://a:1", "placements": [{"shard": 1, "x": 0, "y": 0}]}], "keyspace": {"fallback": {"shard": 1, "x": 0, "y": 0}}`
	if extra == "" {
		return base + "}"
	}
	return base + ", " + extra + "}"
}

// fdtConfigCorpus: config.rs's own #[test] inputs first, then the shipped
// deployment file, then every refusal and edge the parser and resolver
// have, each with the admin token set unless the case is about the
// environment.
func fdtConfigCorpus(t *testing.T) []string {
	t.Helper()
	var lines []string
	add := func(text string) { lines = append(lines, fdtConfigLine(text, fdtAdminEnv)) }
	addEnv := func(text string, env [][2]string) { lines = append(lines, fdtConfigLine(text, env)) }

	// config.rs #[test]s, in file order.
	add(fdtMinimal)
	add(`{
                "backends": [
                    { "id": "db-a", "url": "http://db-a:8892",
                      "placements": [{ "shard": 1, "x": 0, "y": 0 }] }
                ],
                "keyspace": {
                    "rules": [
                        { "kind": "Post", "address_prefix": "Post:",
                          "shard": 9, "x": 4, "y": 4 }
                    ],
                    "fallback": { "shard": 1, "x": 0, "y": 0 }
                }
            }`)
	add(`{
                "backends": [
                    { "id": "db-a", "url": "http://a:1",
                      "placements": [{ "shard": 1, "x": 0, "y": 0 }] },
                    { "id": "db-b", "url": "http://b:1",
                      "placements": [{ "shard": 1, "x": 0, "y": 0 }] }
                ],
                "keyspace": { "fallback": { "shard": 1, "x": 0, "y": 0 } }
            }`)
	secret := `{
                "backends": [
                    { "id": "db-a", "url": "http://a:1",
                      "token_env": "FABRIC_DB_A_TOKEN",
                      "placements": [{ "shard": 1, "x": 0, "y": 0 }] }
                ],
                "keyspace": { "fallback": { "shard": 1, "x": 0, "y": 0 } }
            }`
	add(secret)
	addEnv(fdtMinimal, nil)
	add(`{
                "backends": [
                    { "id": "db-a", "url": "http://a:1",
                      "placements": [{ "shard": 1, "x": 0, "y": 0 }] }
                ],
                "keyspace": { "fallback": { "shard": 1, "x": 0, "y": 0 } },
                "placement_store": "db-a"
            }`)
	add(`{
                "backends": [
                    { "id": "db-a", "url": "http://a:1",
                      "placements": [{ "shard": 1, "x": 0, "y": 0 }] }
                ],
                "keyspace": { "fallback": { "shard": 1, "x": 0, "y": 0 } },
                "cadence": { "liveness_probe_ms": 5000 },
                "silence_budget_ms": 1000
            }`)
	add(`{ "backends": [], "keysapce": {} }`)
	deploy, err := os.ReadFile("../../fabric/deploy/fabric.json")
	if err != nil {
		t.Fatalf("the shipped deployment configuration: %v", err)
	}
	add(string(deploy))
	addEnv(secret, [][2]string{{"FABRIC_ADMIN_TOKEN", "admin-s3cret"}, {"FABRIC_DB_A_TOKEN", "backend-s3cret"}})
	add(`{
                "backends": [
                    { "id": "db-a", "url": "http://a:1",
                      "placements": [{ "shard": 1, "x": 0, "y": 0 }] }
                ],
                "keyspace": { "fallback": { "shard": 1, "x": 0, "y": 0 } },
                "policy": { "phase_timeout_ms": 9000 }
            }`)

	// JSON syntax, at every place serde_json can fail.
	for _, text := range []string{
		``, `   `, "\n\n  ", `{`, `{"backends"`, `{"backends":`, `{"backends": [`, `{"backends": []`,
		`{"backends": [],}`, `{"backends" []}`, `{"backends": [] "x": 1}`, `{,}`, `{"backends": [],, }`,
		`{5: 1}`, `{"backends": [], 5: 1}`, `[]`, `null`, `5`, `-5`, `1.5`, `"x"`, `true`, `false`, `nul`, `tru`,
		`{}`, `{"backends": []} x`, `{"backends": []}}`, `{"backends": [{}]}`, `{"backends": [1,]}`,
		`{"backends": [{"id": "a"} {"id": "b"}]}`, `{"backends": [], "data_listen": "a` + "\x01" + `"}`,
		`{"backends": [], "data_listen": "\q"}`, `{"backends": [], "data_listen": "\u12"}`, `{"backends": [], "data_listen": "\uzzzz"}`,
		`{"backends": [], "data_listen": "\ud800"}`, `{"backends": [], "data_listen": "\ud800x"}`, `{"backends": [], "data_listen": "\ud800\u0041"}`,
		`{"backends": [], "data_listen": "\udc00"}`, `{"backends": [], "data_listen": "\ud83d\ude00"}`, `{"backends": [], "data_listen": "\`,
		`{"backends": [], "drain_ms": 01}`, `{"backends": [], "drain_ms": 1.}`, `{"backends": [], "drain_ms": 1e}`, `{"backends": [], "drain_ms": -}`,
		`{"backends": [], "drain_ms": -x}`, `{"backends": [], "drain_ms": 1e400}`, `{"backends": [], "drain_ms": 1.5e3}`, `{"backends": [], "drain_ms": 99999999999999999999}`,
		`{"backends": [], "drain_ms": 18446744073709551615}`, `{"backends": [], "drain_ms": 18446744073709551616}`, `{"backends": [], "drain_ms": -0}`,
		`{"backends": [], "drain_ms": 1e-400}`, `{"backends": [], "drain_ms": 1.`, `{"backends": [], "drain_ms": 1e+`, `{"backends": [], "drain_ms": 1E5}`,
		`{"backends": [], "drain_ms": 12345678901234567890123.5}`, `{"backends": [], "drain_ms": 0.000000000000000000000000000001e2147483648}`,
		`{"backends": [], "drain_ms": 1e2147483648}`, `{"backends": [], "drain_ms": 0e2147483648}`,
		"{\n  \"backends\": [],\n  \"bogus\": 1\n}", "{\r\n\t\"backends\": 5\r\n}", "\n\n{\"backends\": [\n{\"id\": 7}]}",
	} {
		add(text)
	}

	// Types, shapes and derive semantics.
	for _, text := range []string{
		`{"backends": 5}`, `{"backends": "x"}`, `{"backends": {}}`, `{"backends": null}`, `{"backends": true}`, `{"backends": -1.5}`,
		`{"backends": [5]}`, `{"backends": [[]]}`, `{"backends": [["db-a"]]}`, `{"backends": [["db-a", "http://a:1"]]}`,
		`{"backends": [["db-a", "http://a:1", "r", null, [[1, 0, 0]]]], "keyspace": [[], [1, 0, 0]]}`,
		`{"backends": [["db-a", "http://a:1", "r", null, [[1, 0, 0]], 5]]}`, `{"backends": [["db-a", "http://a:1", "r", null, [[1, 0]]]]}`,
		`{"backends": [{"id": "db-a"}]}`, `{"backends": [{"url": "http://a"}]}`, `{"backends": [{"id": "a", "url": "http://a", "extra": 1}]}`,
		`{"backends": [{"id": "a", "url": "http://a", "id": "b"}]}`, `{"backends": [{"id": 5, "url": "http://a"}]}`,
		`{"backends": [{"id": null, "url": "http://a"}]}`, `{"backends": [{"id": "a", "url": "http://a", "token_env": 5}]}`,
		`{"backends": [{"id": "a", "url": "http://a", "token_env": null}], "keyspace": {"fallback": null}}`,
		`{"backends": [{"id": "a", "url": "http://a", "placements": [{"shard": 1, "x": 256, "y": 0}]}]}`,
		`{"backends": [{"id": "a", "url": "http://a", "placements": [{"shard": 1, "x": -1, "y": 0}]}]}`,
		`{"backends": [{"id": "a", "url": "http://a", "placements": [{"shard": 1, "x": 1.0, "y": 0}]}]}`,
		`{"backends": [{"id": "a", "url": "http://a", "placements": [{"shard": -3, "x": 1, "y": 0}]}]}`,
		`{"backends": [{"id": "a", "url": "http://a", "placements": [{"shard": "1", "x": 1, "y": 0}]}]}`,
		`{"backends": [{"id": "a", "url": "http://a", "placements": [{"shard": 1, "x": 1}]}]}`,
		`{"backends": [{"id": "a", "url": "http://a", "placements": [{"shard": 1, "x": 1, "y": 0, "z": 0}]}]}`,
		`{"backends": [{"id": "a", "url": "http://a", "placements": [{"shard": 1, "x": 1, "y": 0, "x": 2}]}]}`,
		`{"backends": [{"id": "a", "url": "http://a", "placements": {"shard": 1}}]}`,
		`{"backends": [{"id": "a", "url": "http://a", "placements": [null]}]}`,
		`{"backends": [], "keyspace": {"rules": [{"kind": "Post"}]}}`, `{"backends": [], "keyspace": {"rules": {}}}`,
		`{"backends": [], "keyspace": {"fallback": 5}}`, `{"backends": [], "keyspace": {"other": 5}}`, `{"backends": [], "keyspace": 5}`,
		`{"backends": [], "keyspace": {"rules": [{"kind": "P", "address_prefix": "P:", "shard": 1, "x": 0, "y": 0, "q": 1}]}}`,
		`{"backends": [], "cadence": {"liveness_probe_ms": "5"}}`, `{"backends": [], "cadence": {"liveness": 5}}`, `{"backends": [], "cadence": []}`,
		`{"backends": [], "cadence": [1, 2, 3, 4]}`, `{"backends": [], "cadence": [1, 2, 3,]}`, `{"backends": [], "cadence": [1 2]}`,
		`{"backends": [], "policy": {"min_confidence": "high"}}`, `{"backends": [], "policy": {"min_replicas": 1.5}}`, `{"backends": [], "policy": {"min_replicas": -2}}`,
		`{"backends": [], "policy": {"max_decision_age_ms": null, "min_confidence": null}}`, `{"backends": [], "policy": {"retries": 3}}`,
		`{"backends": [], "policy": [1]}`, `{"backends": [], "policy": [null, null, null, null, null, null, null, null, null, null]}`,
		`{"backends": [], "silence_budget_ms": -1}`, `{"backends": [], "silence_budget_ms": "x"}`, `{"backends": [], "probe_timeout_ms": null}`,
		`{"backends": [], "placement_capacity": 2.5}`, `{"backends": [], "placement_store": 5}`, `{"backends": [], "data_listen": 7710}`,
		`{"backends": [], "admin_token_env": ["X"]}`, `{"backends": [], "decision_cooldown_ms": {}}`,
		`{"backends": [], "read_preference": "primary"}`, `{"backends": [], "read_preference": "Primary"}`, `{"backends": [], "read_preference": "prefer_region"}`,
		`{"backends": [], "read_preference": {"prefer_region": {"region": "x"}}}`, `{"backends": [], "read_preference": {"prefer_region": {}}}`,
		`{"backends": [], "read_preference": {"prefer_region": {"region": "x", "zone": 1}}}`, `{"backends": [], "read_preference": {"prefer_region": ["eu"]}}`,
		`{"backends": [], "read_preference": {"prefer_region": null}}`, `{"backends": [], "read_preference": {"primary": null}}`,
		`{"backends": [], "read_preference": {"primary": 5}}`, `{"backends": [], "read_preference": {"bogus": 1}}`, `{"backends": [], "read_preference": {}}`,
		`{"backends": [], "read_preference": {"primary": null, "any_copy": null}}`, `{"backends": [], "read_preference": {"primary" null}}`,
		`{"backends": [], "read_preference": {5: null}}`, `{"backends": [], "read_preference": 5}`, `{"backends": [], "read_preference": ["primary"]}`,
		`{"backends": [], "read_preference": "any_fresh" }`, `{"backends": [], "read_preference": {"any_copy": null} }`, `{"backends": [], "read_preference": {"any_copy": null`,
		`{"backends": [], "read_preference": "prefer_region"}   `, `{"backends": [], "read_preference": "prefer_region", "x": 1}`,
		`{"backends": [], "read_preference": "nope"}`, `{"backends": [], "read_preference": null}`,
		`{"data_listen": "0.0.0.0:1"}`, `{"backends": [], "backends": []}`, `{"backends": [], "cadence": {}, "cadence": {}}`,
		`["0.0.0.0:1", "127.0.0.1:2", "TOKEN", []]`,
		`["0.0.0.0:1", "127.0.0.1:2", "TOKEN", [], {"rules": [], "fallback": null}, [1, 2, 3], {}, null, 1, 2, null, null, 3, 4]`,
		`["0.0.0.0:1", "127.0.0.1:2", "TOKEN", [], {}, [1, 2, 3], {}, null, 1, 2, null, null, 3, 4, 5]`,
	} {
		add(text)
	}

	// Resolution: listen addresses (SocketAddr's parser and Display).
	for _, addr := range []string{
		"0.0.0.0:0", "255.255.255.255:65535", "1.2.3.4:65536", "01.2.3.4:80", "1.2.3.4:080", "1.2.3:80", "1.2.3.4.5:80", "256.1.1.1:80",
		"1.2.3.4", "1.2.3.4:", ":80", "localhost:80", " 1.2.3.4:80", "1.2.3.4:80 ", "1.2.3.4:+80", "1.2.3.4:99999999999",
		"[::]:80", "[::1]:7711", "[::ffff:1.2.3.4]:80", "[::ffff:0102:0304]:80", "[1:2:3:4:5:6:7:8]:1", "[1:0:0:4:0:0:0:8]:1", "[1:0:3:0:5:0:7:0]:1",
		"[0:0:0:0:0:0:0:1]:9", "[fe80::1%3]:80", "[fe80::1%]:80", "[fe80::1%x]:80", "[::1.2.3.4]:80", "[1::2::3]:80", "[1:2:3:4:5:6:7:8:9]:1",
		"[12345::]:1", "[::1]", "::1:80", "[FE80::ABCD]:80", "[1:2:3:4:5:6:1.2.3.4]:5", "[1:2:3:4:5:6:7:1.2.3.4]:5", "[::ffff:1.2.3.4%7]:0",
		"[0:0:0:0:0:ffff:1.2.3.4]:80", "[1::]:80", "[::2:0:0:0:3]:80", "[0000::0001]:80", "[00000::1]:80",
	} {
		b, _ := json.Marshal(addr)
		add(fdtWith(`"data_listen": ` + string(b)))
		add(fdtWith(`"admin_listen": ` + string(b)))
	}

	// Resolution: every other check, in config.rs's order.
	for _, text := range []string{
		`{"backends": []}`,
		`{"backends": [{"id": "", "url": "http://a"}]}`,
		`{"backends": [{"id": "  ", "url": "http://a"}]}`,
		`{"backends": [{"id": "\u00a0\u2003", "url": "http://a"}]}`,
		`{"backends": [{"id": "a", "url": "http://a"}, {"id": "a", "url": "http://b"}]}`,
		`{"backends": [{"id": "a", "url": "http://a"}, {"id": " a", "url": "http://b"}], "keyspace": {"rules": []}}`,
		`{"backends": [{"id": "a", "url": "http://a", "token_env": "T"}]}`,
		`{"backends": [{"id": "a", "url": "http://a", "placements": [{"shard": 1, "x": 12, "y": 0}]}]}`,
		`{"backends": [{"id": "a", "url": "http://a", "placements": [{"shard": 18446744073709551615, "x": 0, "y": 13}]}]}`,
		`{"backends": [{"id": "a", "url": "http://a", "placements": [{"shard": 1, "x": 0, "y": 0}, {"shard": 1, "x": 0, "y": 0}]}]}`,
		`{"backends": [{"id": "a", "url": "ftp://a", "placements": [{"shard": 1, "x": 0, "y": 0}]}]}`,
		`{"backends": [{"id": "a", "url": "  https://a///  ", "placements": [{"shard": 1, "x": 0, "y": 0}]}], "keyspace": {"fallback": {"shard": 1, "x": 0, "y": 0}}}`,
		`{"backends": [{"id": "a", "url": "//", "placements": []}]}`,
		`{"backends": [{"id": "a", "url": "HTTP://a"}]}`,
		`{"backends": [{"id": "a", "url": "http://a", "placements": [{"shard": 1, "x": 0, "y": 1}]}], "keyspace": {"rules": [{"kind": "P", "address_prefix": "P:", "shard": 1, "x": 12, "y": 0}]}}`,
		`{"backends": [{"id": "a", "url": "http://a", "placements": [{"shard": 1, "x": 0, "y": 1}]}], "keyspace": {"fallback": {"shard": 1, "x": 12, "y": 0}}}`,
		`{"backends": [{"id": "a", "url": "http://a", "placements": [{"shard": 1, "x": 0, "y": 0}]}], "keyspace": {"rules": [{"kind": "", "address_prefix": "P:", "shard": 1, "x": 0, "y": 0}]}}`,
		`{"backends": [{"id": "a", "url": "http://a", "placements": [{"shard": 1, "x": 0, "y": 0}]}], "keyspace": {"rules": [{"kind": "P", "address_prefix": "", "shard": 1, "x": 0, "y": 0}]}}`,
		`{"backends": [{"id": "a", "url": "http://a", "placements": [{"shard": 1, "x": 0, "y": 0}]}], "keyspace": {"rules": [{"kind": "P", "address_prefix": "P:", "shard": 1, "x": 0, "y": 0}, {"kind": "P", "address_prefix": "Q:", "shard": 1, "x": 0, "y": 0}]}}`,
		`{"backends": [{"id": "a", "url": "http://a", "placements": [{"shard": 1, "x": 0, "y": 0}]}], "keyspace": {"rules": [{"kind": "P", "address_prefix": "P:", "shard": 1, "x": 0, "y": 0}, {"kind": "Q", "address_prefix": "P:", "shard": 1, "x": 0, "y": 0}]}}`,
		`{"backends": [{"id": "a", "url": "http://a", "placements": [{"shard": 1, "x": 0, "y": 0}, {"shard": 2, "x": 3, "y": 4}]}], "keyspace": {"rules": [{"kind": "P", "address_prefix": "P:", "shard": 2, "x": 3, "y": 4}, {"kind": "Q", "address_prefix": "Q:", "shard": 1, "x": 0, "y": 0}]}}`,
		`{"backends": [{"id": "a", "url": "http://a", "placements": [{"shard": 1, "x": 0, "y": 0}]}], "keyspace": {"rules": [{"kind": "P", "address_prefix": "P:", "shard": 1, "x": 0, "y": 0}], "fallback": {"shard": 1, "x": 0, "y": 0}}}`,
		`{"backends": [{"id": "a", "url": "http://a", "placements": [{"shard": 1, "x": 0, "y": 0}]}], "keyspace": {"fallback": {"shard": 2, "x": 0, "y": 0}}}`,
		`{"backends": [{"id": "a", "url": "http://a", "placements": [{"shard": 1, "x": 0, "y": 0}]}], "keyspace": {}}`,
		`{"backends": [{"id": "a", "url": "http://a", "placements": [{"shard": 1, "x": 0, "y": 0}]}]}`,
		`{"backends": [{"id": "a", "url": "http://a", "placements": [{"shard": 1, "x": 0, "y": 0}]}], "keyspace": {"fallback": {"shard": 1, "x": 0, "y": 0}}, "placement_store": "zz"}`,
		`{"backends": [{"id": "b", "url": "https://b", "region": "eu", "token_env": "B", "placements": [{"shard": 3, "x": 11, "y": 12}, {"shard": 3, "x": 0, "y": 12}]}, {"id": "a", "url": "http://a", "placements": [{"shard": 1, "x": 0, "y": 0}]}], "keyspace": {"fallback": {"shard": 1, "x": 0, "y": 0}, "rules": [{"kind": "Post", "address_prefix": "Post:", "shard": 3, "x": 11, "y": 12}]}, "placement_store": "b", "read_preference": {"prefer_region": {"region": "eu"}}}`,
	} {
		add(text)
	}
	addEnv(`{"backends": [{"id": "a", "url": "http://a", "token_env": "T", "placements": [{"shard": 1, "x": 0, "y": 0}]}], "keyspace": {"fallback": {"shard": 1, "x": 0, "y": 0}}}`,
		[][2]string{{"FABRIC_ADMIN_TOKEN", "x"}, {"T", ""}})
	addEnv(`{"backends": [{"id": "b", "url": "https://b", "region": "eu", "token_env": "B", "placements": [{"shard": 3, "x": 11, "y": 12}]}], "keyspace": {"fallback": {"shard": 3, "x": 11, "y": 12}}, "placement_store": "b", "admin_token_env": "ADMIN"}`,
		[][2]string{{"ADMIN", "first"}, {"B", "bee"}, {"ADMIN", "second"}})
	addEnv(fdtMinimal, [][2]string{{"FABRIC_ADMIN_TOKEN", ""}})
	addEnv(fdtWith(`"admin_token_env": "OTHER"`), [][2]string{{"FABRIC_ADMIN_TOKEN", "x"}})
	for _, extra := range []string{
		`"cadence": {"liveness_probe_ms": 0}`, `"cadence": {"telemetry_poll_ms": 0}`, `"cadence": {"control_cycle_ms": 0}`,
		`"cadence": {"liveness_probe_ms": 0, "control_cycle_ms": 0}`, `"cadence": {"liveness_probe_ms": 7}`,
		`"cadence": {"liveness_probe_ms": 18446744073709551615}`, `"cadence": {"liveness_probe_ms": 6148914691236517205}`,
		`"cadence": {"liveness_probe_ms": 6148914691236517206}`, `"silence_budget_ms": 5000`, `"silence_budget_ms": 4999`,
		`"silence_budget_ms": 0, "cadence": {"liveness_probe_ms": 1}`, `"silence_budget_ms": null`,
		`"probe_timeout_ms": 0`, `"placement_capacity": 0`, `"placement_capacity": 18446744073709551615`, `"drain_ms": 0`,
		`"decision_cooldown_ms": 18446744073709551615`, `"read_preference": "any_copy"`, `"read_preference": {"prefer_region": {"region": ""}}`,
		`"policy": {"max_decision_age_ms": 1, "min_confidence": 0.5, "min_replicas": 3, "max_node_utilization": 1, "max_concurrent_actions": 0, "max_actions_per_node": 18446744073709551615, "phase_timeout_ms": 2, "measurement_settle_ms": 3, "measurement_deadline_ms": 4, "outcome_noise_floor": -0.0}`,
		`"policy": {"min_confidence": 1e-7, "outcome_noise_floor": 12345678.9}`,
		`"data_listen": "[::1]:7710", "admin_listen": "[fe80::1%2]:7711"`,
	} {
		add(fdtWith(extra))
	}
	return lines
}

func TestFabricDaemonConfig(t *testing.T) {
	inputs := fdtConfigCorpus(t)
	golden := fdtGolden(t, "fabric_daemon_config.golden")
	port := fdtRun(t, "daemon-config", inputs)
	fdtCompare(t, "config", inputs, port, golden)
}

func TestFabricDaemonConfigGoldenMatchesRust(t *testing.T) {
	fdtLiveGolden(t, "daemon-config", fdtConfigCorpus(t), "fabric_daemon_config.golden")
}

// config.rs's #[test]s, asserted on the port's own answers (corpus lines
// 0..10 are those tests' inputs, in order).
func TestFabricDaemonConfigRustTests(t *testing.T) {
	out := fdtRun(t, "daemon-config", fdtConfigCorpus(t)[:11])
	has := func(i int, want ...string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains(out[i], w) {
				t.Errorf("case %d: %q lacks %q", i, out[i], w)
			}
		}
	}
	// a_minimal_declaration_resolves_to_a_servable_fleet
	has(0, "OK\t", "data=0.0.0.0:7710 ", "admin=127.0.0.1:7711 ", "token=admin-secret ", "|us-east|~|", "topology=[1/0,0@db-a/us-east]", "span=1/0,0 ", "silence=15000 ", "cadence=5000,")
	// a_keyspace_rule_for_an_unheld_cell_is_refused_at_startup
	has(1, "ERR\t", "shard 9 (4,4)", "no backend declares")
	// one_cell_may_not_be_declared_on_two_instances
	has(2, "ERR\t", "exactly one holder")
	// a_missing_secret_names_the_variable_and_never_a_value
	has(3, "ERR\t", "FABRIC_DB_A_TOKEN", "db-a")
	// the_admin_surface_is_never_served_without_a_token
	has(4, "ERR\t", "FABRIC_ADMIN_TOKEN", "unauthenticated")
	// a_placement_store_without_a_credential_is_refused
	has(5, "ERR\t", "token_env")
	// a_silence_budget_shorter_than_a_probe_interval_is_refused
	has(6, "ERR\t", "between probes")
	// an_unknown_key_is_a_startup_error
	has(7, "PARSE\t", "keysapce")
	// the_shipped_deployment_configuration_still_resolves: two backends,
	// one place, no credentials.
	has(8, "OK\t", "span=1/0,0 ", "|a|~|", "|b|~|")
	if strings.Count(out[8], "|~|") != 2 {
		t.Errorf("deploy: %q", out[8])
	}
	// debug_never_prints_a_secret: the tokens are loaded (and the redacted
	// rendering is checked in TestFabricDaemonSettingsDebug).
	has(9, "OK\t", "token=admin-s3cret ", "|backend-s3cret|")
	// policy_thresholds_come_from_the_file_and_default_conservatively
	has(10, "OK\t", "policy=15000,0.8,1,0.85,8,2,9000,30000,")
}

// fdtDebugCorpus: every config case that resolves with at most one
// placement (the topology is a HashMap in the crate, so its Debug order is
// fixed only then), plus every refusal.
func fdtDebugCorpus(t *testing.T) []string {
	t.Helper()
	inputs := fdtConfigCorpus(t)
	golden := fdtGolden(t, "fabric_daemon_config.golden")
	var out []string
	for i, line := range golden {
		if i >= len(inputs) {
			break
		}
		if strings.HasPrefix(line, "OK\t") {
			_, rest, _ := strings.Cut(line, "topology=[")
			topo, _, _ := strings.Cut(rest, "]")
			if strings.Contains(topo, " ") {
				continue
			}
		}
		out = append(out, inputs[i])
	}
	return out
}

func TestFabricDaemonSettingsDebug(t *testing.T) {
	inputs := fdtDebugCorpus(t)
	port := fdtRun(t, "daemon-settings-debug", inputs)
	fdtCompare(t, "settings debug", inputs, port, fdtGolden(t, "fabric_daemon_settings_debug.golden"))
	// debug_never_prints_a_secret: corpus line 9 carries both secrets.
	debugLine := fdtRun(t, "daemon-settings-debug", fdtConfigCorpus(t)[9:10])[0]
	for _, secret := range []string{"admin-s3cret", "backend-s3cret"} {
		if strings.Contains(debugLine, secret) {
			t.Errorf("Debug prints %q: %s", secret, debugLine)
		}
	}
	if !strings.Contains(debugLine, "<redacted>") {
		t.Errorf("Debug redacts nothing: %s", debugLine)
	}
}

func TestFabricDaemonSettingsDebugGoldenMatchesRust(t *testing.T) {
	fdtLiveGolden(t, "daemon-settings-debug", fdtDebugCorpus(t), "fabric_daemon_settings_debug.golden")
}

// ---------------------------------------------------------------- status

func fdtSnapshot(taken uint64, ops, lat float64, depth uint64, score float64, pressure string) map[string]any {
	return map[string]any{
		"taken_at_ms": taken, "operations_per_second": ops, "read_ratio": 0.7, "write_ratio": 0.30000000000000004,
		"read_latency_us": lat, "write_latency_us": lat / 3, "cpu_utilization": 0.1 + 0.2, "memory_utilization": 1.0 / 3.0,
		"queue_depth": depth, "pressure_score": score, "pressure": pressure,
	}
}

func fdtReport(id uint64, action any, label, verdict string, measurement any, failure any) map[string]any {
	return map[string]any{
		"action_id": id, "target": map[string]any{"shard_id": id * 7, "coordinate": map[string]any{"x": id % 12, "y": id % 13}},
		"action": action, "action_label": label, "mechanism": "placement",
		"confidence": 0.93, "expected_gain": 0.25, "estimated_cost": 1e-7, "realized_gain": -0.0, "gain_ratio": 1e21,
		"verdict": verdict, "admitted_at_ms": 1_700_000_000_000 + id, "concluded_at_ms": uint64(18446744073709551615),
		"measurement": measurement, "failure": failure,
	}
}

// fdtStatusCorpus: GET /status bodies covering every Option both ways,
// every OptimizationAction / OutcomeVerdict / PressureLevel variant, u64
// extremes, awkward floats and strings serde_json must escape.
func fdtStatusCorpus() []string {
	starting := map[string]any{
		"version": "0.1.0", "started_at_ms": 1700000000000, "snapshot_at_ms": 1700000000000, "clock_ms": 0, "cycles": 0,
		"draining": false, "data_listen": "0.0.0.0:7710", "admin_listen": "127.0.0.1:7711", "routing_generation": 0,
		"placement_generation": 0, "telemetry_source": "starting", "observations": 0, "profiles": 0, "hot_cells": 0,
		"keyspace": map[string]any{"rules": []any{}, "fallback": nil, "spans_one_place": false},
		"backends": []any{}, "placements": []any{}, "in_flight": []any{},
		"decisions":       map[string]any{"proposed": 0, "admitted": 0, "refused": 0, "last_refusal": nil},
		"placement_store": map[string]any{"configured": nil, "state": "starting", "last_error": nil, "writes": 0},
		"movers":          map[string]any{"copying": []any{}, "refused": []any{}}, "history": []any{},
	}
	measured := map[string]any{
		"before": fdtSnapshot(10, 12000, 40000, 30000, 0.91, "Critical"), "after": fdtSnapshot(20, 3000.5, 5000, 0, 0.1, "Normal"),
		"expected_gain": 0.5, "realized_gain": 0.81, "gain_ratio": 1.62, "latency_delta_us": -52500, "throughput_delta": -8999.5, "verdict": "Improved",
	}
	measured2 := map[string]any{
		"before": fdtSnapshot(0, 0, 0, 18446744073709551615, 0.5, "Elevated"), "after": fdtSnapshot(1, 5e-324, 1.7976931348623157e308, 1, 0.75, "High"),
		"expected_gain": 0, "realized_gain": -0.25, "gain_ratio": 0, "latency_delta_us": 1e-5, "throughput_delta": 123456789012345680000.0, "verdict": "Regressed",
	}
	full := map[string]any{
		"version": "0.1.0", "started_at_ms": 1700000000000, "snapshot_at_ms": 1700000012345, "clock_ms": 1700000012000, "cycles": 42,
		"draining": true, "data_listen": "[::1]:7710", "admin_listen": "127.0.0.1:7711", "routing_generation": 9,
		"placement_generation": 3, "telemetry_source": "fabric-simulator (2 instances, write-heavy)", "observations": 1024, "profiles": 2, "hot_cells": 1,
		"keyspace": map[string]any{"rules": []any{"Post / Post:* -> 1/0,0", "User / U\"ser:* -> 18446744073709551615/11,12"}, "fallback": "1/0,0", "spans_one_place": true},
		"backends": []any{
			map[string]any{"id": "us-east-db-0", "url": "http://127.0.0.1:1234", "region": "us-east", "availability": "serviceable", "health": "healthy",
				"last_probe": "serving (200)", "last_probe_at_ms": 1700000012000, "silence_ms": 0, "heartbeats": 17, "telemetry": true, "last_sample": "sampled"},
			map[string]any{"id": "db-é\U0001F600", "url": "http://b", "region": "", "availability": "unknown", "health": "unreachable",
				"last_probe": "not probed yet", "last_probe_at_ms": nil, "silence_ms": nil, "heartbeats": 0, "telemetry": false, "last_sample": nil},
			map[string]any{"id": "tab\tnl\nctl\u0001del\u007f", "url": "", "region": "r\\", "availability": "unreachable", "health": "degraded",
				"last_probe": "unreachable: timed out", "last_probe_at_ms": uint64(18446744073709551615), "silence_ms": 99999, "heartbeats": 1, "telemetry": true, "last_sample": "failed: facetql unreachable: x"},
		},
		"placements": []any{
			map[string]any{"shard": 1, "x": 0, "y": 0, "holder": "us-west-db-0", "region": "us-west", "migration": map[string]any{
				"phase": "cutover", "source": "us-east-db-0", "destination": "us-west-db-0", "read_owner": "us-west-db-0", "write_fenced": true, "has_cut_over": true}},
			map[string]any{"shard": 2, "x": 11, "y": 12, "holder": "b", "region": "b", "migration": nil},
		},
		"in_flight": []any{
			map[string]any{"id": 1, "shard": 1, "x": 0, "y": 0, "action": "move -> us-west-db-0", "mechanism": "placement", "state": "running",
				"phase": "transfer", "fraction": 0.5, "source": "us-east-db-0", "destination": "us-west-db-0", "admitted_at_ms": 1700000001000,
				"has_cut_over": false, "bytes_copied": 4096, "resident_bytes": 8192},
			map[string]any{"id": 2, "shard": 3, "x": 4, "y": 5, "action": "isolate", "mechanism": "placement", "state": "admitted",
				"phase": nil, "fraction": nil, "source": "a", "destination": nil, "admitted_at_ms": 0, "has_cut_over": true, "bytes_copied": 0, "resident_bytes": nil},
			map[string]any{"id": 3, "shard": 3, "x": 4, "y": 6, "action": "replicate -> c", "mechanism": "placement", "state": "running",
				"phase": "verify", "fraction": 1, "source": "a", "destination": "c", "admitted_at_ms": 5, "has_cut_over": false, "bytes_copied": 1, "resident_bytes": 0},
		},
		"decisions":       map[string]any{"proposed": 5, "admitted": 3, "refused": 2, "last_refusal": "decision is stale: observed at 1 ms, now 20000 ms"},
		"placement_store": map[string]any{"configured": "us-east-db-0", "state": "loaded 2 placement(s) from 'us-east-db-0'", "last_error": "could not persist shard 1 (0,0) on 'x': facetql compare-and-set refused (412): \"etag\"", "writes": 7},
		"movers": map[string]any{
			"copying": []any{map[string]any{"id": 1, "destination": "us-west-db-0"}},
			"refused": []any{map[string]any{"id": 2, "reason": "'a' has no configured credential"}, map[string]any{"id": 3, "reason": ""}},
		},
		"history": []any{
			fdtReport(1, map[string]any{"Move": map[string]any{"target": "us-west-db-0"}}, "move -> us-west-db-0", "Improved", measured, nil),
			fdtReport(2, map[string]any{"Replicate": map[string]any{"target": "c"}}, "replicate -> c", "Regressed", measured2, nil),
			fdtReport(3, "NoAction", "no-action", "Failed", nil, "phase transfer timed out after 60000 ms"),
			fdtReport(4, "Split", "split", "RolledBack", nil, "rolled back"),
			fdtReport(5, "Isolate", "isolate", "NotMeasured", nil, nil),
			fdtReport(6, map[string]any{"Colocate": map[string]any{"target": map[string]any{"x": 11, "y": 12}}}, "colocate -> (11,12)", "Unchanged", nil, nil),
		},
	}
	var out []string
	for _, doc := range []map[string]any{starting, full} {
		b, err := json.Marshal(doc)
		if err != nil {
			panic(err)
		}
		out = append(out, string(b))
	}
	return out
}

func TestFabricDaemonStatus(t *testing.T) {
	inputs := fdtStatusCorpus()
	port := fdtRun(t, "daemon-status", inputs)
	fdtCompare(t, "status", inputs, port, fdtGolden(t, "fabric_daemon_status.golden"))
}

// fdtStatusRereadCorpus: the status bodies the crate itself wrote (the
// first column of the status golden file).
func fdtStatusRereadCorpus(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, line := range fdtGolden(t, "fabric_daemon_status.golden") {
		body, _, _ := strings.Cut(line, "\t")
		out = append(out, body)
	}
	return out
}

// Both ways: the crate's own GET /status bodies (byte-identical to the
// port's, by TestFabricDaemonStatus) read back by the port and by the
// crate, and written again. serde_json's default float parsing is not
// round-trip exact, so this is compared against the crate re-reading its
// own output, never against the input.
func TestFabricDaemonStatusReread(t *testing.T) {
	inputs := fdtStatusRereadCorpus(t)
	port := fdtRun(t, "daemon-status", inputs)
	fdtCompare(t, "status reread", inputs, port, fdtGolden(t, "fabric_daemon_status_reread.golden"))
}

func TestFabricDaemonStatusRereadGoldenMatchesRust(t *testing.T) {
	fdtLiveGolden(t, "daemon-status", fdtStatusRereadCorpus(t), "fabric_daemon_status_reread.golden")
}

func TestFabricDaemonStatusGoldenMatchesRust(t *testing.T) {
	fdtLiveGolden(t, "daemon-status", fdtStatusCorpus(), "fabric_daemon_status.golden")
}

// ---------------------------------------------------------------- liveness

func fdtProbeCorpus() []string {
	return []string{
		"Serving|200|", "Serving|204|", "Serving|299|", "Answering|503|", "Answering|404|", "Answering|301|", "Answering|100|",
		"Unreachable|0|timed out", "Unreachable|0|connection refused or unresolvable", "Unreachable|0|", "Unreachable|0|a|b",
	}
}

func TestFabricDaemonProbe(t *testing.T) {
	inputs := fdtProbeCorpus()
	port := fdtRun(t, "daemon-probe", inputs)
	fdtCompare(t, "probe", inputs, port, fdtGolden(t, "fabric_daemon_probe.golden"))
	// only_a_reachable_instance_files_a_heartbeat
	for i, want := range map[int]string{0: "true|true|", 3: "true|false|", 7: "false|false|"} {
		if !strings.HasPrefix(port[i], want) {
			t.Errorf("probe %s: %s, want %s...", inputs[i], port[i], want)
		}
	}
}

func TestFabricDaemonProbeGoldenMatchesRust(t *testing.T) {
	fdtLiveGolden(t, "daemon-probe", fdtProbeCorpus(), "fabric_daemon_probe.golden")
}

// fdtFleet starts the instances a sweep meets: serving, answering badly,
// wedged past the probe timeout, hanging up without a response, answering
// with something that is not HTTP, and a port nothing listens on.
func fdtFleet(t *testing.T) (map[string]string, func()) {
	t.Helper()
	var servers []*httptest.Server
	start := func(h http.HandlerFunc) string {
		ts := httptest.NewServer(h)
		servers = append(servers, ts)
		return ts.URL
	}
	urls := map[string]string{
		"ok":  start(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("FacetQL Online")) }),
		"bad": start(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }),
		"wedged": start(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(3 * time.Second):
			}
		}),
		"hangup": start(func(w http.ResponseWriter, r *http.Request) {
			c, _, _ := w.(http.Hijacker).Hijack()
			c.Close()
		}),
		"garbage": start(func(w http.ResponseWriter, r *http.Request) {
			c, _, _ := w.(http.Hijacker).Hijack()
			c.Write([]byte("this is not http\r\n\r\n"))
			c.Close()
		}),
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	urls["dead"] = "http://" + ln.Addr().String()
	ln.Close()
	return urls, func() {
		for _, ts := range servers {
			ts.Close()
		}
	}
}

func fdtSweepLine(timeoutMs int, ids []string, urls map[string]string) string {
	targets := [][]string{}
	for _, id := range ids {
		targets = append(targets, []string{id, urls[id]})
	}
	b, _ := json.Marshal(map[string]any{"timeout_ms": timeoutMs, "targets": targets})
	return string(b)
}

// A real sweep, port and crate, over the same live fleet — including
// nothing_listening_is_unreachable_rather_than_unhealthy (127.0.0.1:1).
func TestFabricDaemonSweep(t *testing.T) {
	urls, stop := fdtFleet(t)
	defer stop()
	urls["port1"] = "http://127.0.0.1:1"
	inputs := []string{
		fdtSweepLine(500, []string{"port1"}, urls),
		fdtSweepLine(400, []string{"ok", "bad", "wedged", "hangup", "garbage", "dead"}, urls),
		fdtSweepLine(400, []string{}, urls),
	}
	started := time.Now()
	port := fdtRun(t, "daemon-sweep", inputs)
	if took := time.Since(started); took > 2500*time.Millisecond {
		t.Errorf("the sweep took %v: its probes did not run concurrently", took)
	}
	want := []string{
		"port1=unreachable: connection refused or unresolvable",
		"ok=serving (200);bad=answering 503;wedged=unreachable: timed out;hangup=unreachable: the request could not be sent;garbage=unreachable: the request could not be sent;dead=unreachable: connection refused or unresolvable",
		"",
	}
	fdtCompare(t, "sweep (expected)", inputs, port, want)
	fdtCompare(t, "sweep (crate)", inputs, port, laRust(t, "daemon-sweep", inputs))
}

// ---------------------------------------------------------------- telemetry

const fdtStatsBody = `{"node_count":3,"edge_count":1,"user_count":1,"history_entries":4,"kinds":[{"kind":"Post","count":3}],"reads_total":10,"writes_total":5,"storage":{"page_size":4096,"segments":1,"pages":2,"obsolete_bytes":0},"version":"0.13.0"}`

// The live telemetry source against a fleet: a credentialed instance that
// answers /stats (a baseline on the first poll), one that refuses the
// token, one that is gone, one with no credential (no target at all) and
// one with a credential but no cell (no target either).
func TestFabricDaemonTelemetry(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stats" && r.Header.Get("x-api-key") == "tok-a" {
			w.Header().Set("content-type", "application/json")
			w.Write([]byte(fdtStatsBody))
			return
		}
		w.WriteHeader(404)
	}))
	defer good.Close()
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte("invalid api key"))
	}))
	defer refusing.Close()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := "http://" + ln.Addr().String()
	ln.Close()
	config := fmt.Sprintf(`{"backends": [
		{"id": "a", "url": %q, "token_env": "TA", "placements": [{"shard": 1, "x": 0, "y": 0}, {"shard": 1, "x": 1, "y": 0}]},
		{"id": "b", "url": %q, "token_env": "TB", "placements": [{"shard": 2, "x": 3, "y": 4}]},
		{"id": "c", "url": %q, "token_env": "TC", "region": "eu", "placements": [{"shard": 3, "x": 0, "y": 0}]},
		{"id": "d", "url": "http://d:1", "placements": [{"shard": 4, "x": 0, "y": 0}]},
		{"id": "e", "url": "http://e:1", "token_env": "TE"}
	], "keyspace": {"fallback": {"shard": 1, "x": 0, "y": 0}}}`, good.URL, refusing.URL, dead)
	env := [][2]string{{"FABRIC_ADMIN_TOKEN", "x"}, {"TA", "tok-a"}, {"TB", "tok-b"}, {"TC", "tok-c"}, {"TE", "tok-e"}}
	inputs := []string{fdtConfigLine(config, env), fdtConfigLine(fdtMinimal, fdtAdminEnv)}
	port := fdtRun(t, "daemon-telemetry", inputs)
	if !strings.HasPrefix(port[0], "facetql /stats over 3 instance(s)|0|a=baseline taken;b=failed: facetql refused the token (401)") ||
		!strings.Contains(port[0], ";c=failed: facetql unreachable: ") || port[1] != "facetql /stats over 0 instance(s)|0|" {
		t.Errorf("telemetry: %q", port)
	}
	rust := laRust(t, "daemon-telemetry", inputs)
	// The unreachable instance's transport text is each HTTP stack's own
	// (reqwest's error chain vs the runtime's dial error); everything
	// before it is compared exactly.
	for i := range inputs {
		p, r := port[i], rust[i]
		if k := strings.Index(p, "c=failed: facetql unreachable: "); k >= 0 {
			p = p[:k]
		}
		if k := strings.Index(r, "c=failed: facetql unreachable: "); k >= 0 {
			r = r[:k]
		}
		if p != r {
			t.Errorf("telemetry %d:\n port %s\n rust %s", i, port[i], rust[i])
		}
	}
}

// ---------------------------------------------------------------- control

const (
	fdtSource      = "us-east-db-0"
	fdtDestination = "us-west-db-0"
)

// fdtControlConfig is tests/daemon.rs's declaration (its fake FacetQLs'
// addresses fixed), with `tokens` giving both instances a credential so the
// daemon's own mover can be started.
func fdtControlConfig(tokens bool) string {
	tok := ""
	if tokens {
		tok = `"token_env": "T",`
	}
	return fmt.Sprintf(`{
		"data_listen": "127.0.0.1:0", "admin_listen": "127.0.0.1:0",
		"backends": [
			{"id": %q, "url": "http://127.0.0.1:9101", "region": "us-east", %s "placements": [{"shard": 1, "x": 0, "y": 0}]},
			{"id": %q, "url": "http://127.0.0.1:9102", "region": "us-west", %s "placements": [{"shard": 2, "x": 0, "y": 0}]}
		],
		"keyspace": {"rules": [{"kind": "Post", "address_prefix": "Post:", "shard": 1, "x": 0, "y": 0}], "fallback": {"shard": 1, "x": 0, "y": 0}},
		"cadence": {"liveness_probe_ms": 100, "telemetry_poll_ms": 100, "control_cycle_ms": 50},
		"silence_budget_ms": 5000, "probe_timeout_ms": 300,
		"policy": {"measurement_settle_ms": 0, "phase_timeout_ms": 60000},
		"drain_ms": 5000
	}`, fdtSource, tok, fdtDestination, tok)
}

// fdtDuress: the observation of a cell under sustained write pressure that
// tests/daemon.rs feeds (the same numbers fabric-runtime's loop test uses).
func fdtDuress(atMs int64) string {
	return fmt.Sprintf(`{"Telemetry":{"timestamp_ms":%d,"node_id":%q,"shard":{"id":1,"workload_domain":"us-east"},"samples":[{"coordinate":{"x":0,"y":0},"operations_per_second":12000.0,"read_ratio":0.1,"write_ratio":0.9,"read_latency_us":40000.0,"write_latency_us":60000.0,"cpu_utilization":0.99,"memory_utilization":0.95,"queue_depth":30000,"cell_breakdown":[],"cell_breakdown_partial":false}]}}`, atMs, fdtSource)
}

type fdtScript struct {
	steps []map[string]any
}

func (s *fdtScript) add(step map[string]any) { s.steps = append(s.steps, step) }

// cycle at `now`, both instances answering as `dest` says for the
// destination, the hot sample when `hot`.
func fdtCycleStep(now int64, dest string, hot bool) map[string]any {
	destProbe := []any{fdtDestination, "Serving", 200, ""}
	switch dest {
	case "answering":
		destProbe = []any{fdtDestination, "Answering", 503, ""}
	case "gone":
		destProbe = []any{fdtDestination, "Unreachable", 0, "connection refused or unresolvable"}
	}
	messages := []any{}
	notes := []any{}
	if hot {
		messages = append(messages, fdtDuress(now))
		notes = append(notes, []any{fdtSource, "sampled"})
	}
	return map[string]any{
		"op": "cycle", "now": now,
		"probes": []any{[]any{fdtSource, "Serving", 200, ""}, destProbe},
		"sample": map[string]any{"messages": messages, "notes": notes},
	}
}

func fdtScriptLine(tokens bool, s fdtScript) string {
	env := [][]string{{"FABRIC_ADMIN_TOKEN", "operator-secret"}}
	if tokens {
		env = append(env, []string{"T", "tok"})
	}
	b, err := json.Marshal(map[string]any{
		"config": fdtControlConfig(tokens), "env": env,
		"telemetry": "fabric-simulator (2 instances, write-heavy)", "steps": s.steps,
	})
	if err != nil {
		panic(err)
	}
	return string(b)
}

func fdtVerified(rows, at int64) map[string]any {
	return map[string]any{"kind": "Verified", "rows": rows, "at_ms": at}
}

// fdtControlCorpus: tests/daemon.rs's two scenarios as scripts, plus the
// operator requests' every refusal and the daemon's own mover.
func fdtControlCorpus() []string {
	const t0 = int64(1_700_000_000_000)
	var out []string

	// the_daemon_boots_routes_traffic_and_follows_a_control_loop_decision
	{
		var s fdtScript
		s.add(map[string]any{"op": "boot", "now": t0})
		now := t0 + 5
		for i := 0; i < 8; i++ {
			s.add(fdtCycleStep(now, "serving", true))
			now += 50
		}
		s.add(map[string]any{"op": "transfer", "id": 1, "atoms": 1, "bytes": 4096, "resident": 4096, "now": now})
		for i := 0; i < 8; i++ {
			now += 50
			s.add(fdtCycleStep(now, "serving", true))
		}
		s.add(map[string]any{"op": "abort", "id": 1, "now": now + 1})
		for i := 0; i < 4; i++ {
			now += 50
			s.add(fdtCycleStep(now, "answering", true))
		}
		for i := 0; i < 6; i++ {
			now += 1500
			s.add(fdtCycleStep(now, "gone", true))
		}
		s.add(map[string]any{"op": "drain", "now": now + 10, "cycles": []any{fdtCycleStep(now+60, "gone", true)}})
		out = append(out, fdtScriptLine(false, s))
	}

	// shutdown_rolls_back_an_action_that_has_not_cut_over
	{
		var s fdtScript
		s.add(map[string]any{"op": "boot", "now": t0})
		now := t0 + 5
		for i := 0; i < 5; i++ {
			s.add(fdtCycleStep(now, "serving", true))
			now += 50
		}
		s.add(map[string]any{"op": "drain", "now": now, "cycles": []any{fdtCycleStep(now+20, "serving", true), fdtCycleStep(now+40, "serving", true)}})
		out = append(out, fdtScriptLine(false, s))
	}

	// Every operator refusal, an abort short of the cutover, and a drain
	// that runs out of budget with an action past its cutover.
	{
		var s fdtScript
		s.add(map[string]any{"op": "boot", "now": t0})
		s.add(fdtCycleStep(t0, "serving", false))
		s.add(map[string]any{"op": "transfer", "id": 99, "atoms": 1, "bytes": 1, "resident": nil, "now": t0 + 1})
		s.add(map[string]any{"op": "abort", "id": 99, "now": t0 + 2})
		s.add(map[string]any{"op": "copy", "id": 99, "now": t0 + 3, "report": map[string]any{"rows": 0, "bytes": 0, "resident": nil, "snapshot_complete": false, "observed": 0, "applied": 0, "verdict": map[string]any{"kind": "Pending"}}})
		now := t0 + 5
		for i := 0; i < 5; i++ {
			s.add(fdtCycleStep(now, "serving", true))
			now += 50
		}
		s.add(map[string]any{"op": "abort", "id": 1, "now": now})
		s.add(map[string]any{"op": "abort", "id": 1, "now": now + 1})
		s.add(map[string]any{"op": "transfer", "id": 1, "atoms": 5, "bytes": uint64(18446744073709551615), "resident": nil, "now": now + 2})
		for i := 0; i < 4; i++ {
			now += 50
			s.add(fdtCycleStep(now, "serving", true))
		}
		s.add(map[string]any{"op": "drain", "now": now, "cycles": []any{fdtCycleStep(now+20, "serving", false), fdtCycleStep(now+6000, "serving", false)}})
		out = append(out, fdtScriptLine(false, s))
	}

	// With credentials the daemon's own mover takes the copy: progress
	// reports, the write gap, a failed then a verified check, and the drain
	// of a copy past its cutover whose budget runs out.
	{
		var s fdtScript
		s.add(map[string]any{"op": "boot", "now": t0})
		now := t0 + 5
		for i := 0; i < 5; i++ {
			s.add(fdtCycleStep(now, "serving", true))
			now += 50
		}
		rep := func(rows, bytes int64, resident any, done bool, observed, applied int64, verdict map[string]any) map[string]any {
			return map[string]any{"rows": rows, "bytes": bytes, "resident": resident, "snapshot_complete": done, "observed": observed, "applied": applied, "verdict": verdict}
		}
		s.add(map[string]any{"op": "copy", "id": 1, "now": now, "report": rep(10, 2048, nil, false, 3, 0, map[string]any{"kind": "Pending"})})
		s.add(map[string]any{"op": "copy", "id": 1, "now": now + 1, "report": rep(20, 4096, 8192, true, 5, 7, map[string]any{"kind": "Failed", "reason": "row Post:1 differs in `owner`", "at_ms": now + 1})})
		s.add(fdtCycleStep(now+50, "serving", true))
		s.add(map[string]any{"op": "copy", "id": 1, "now": now + 60, "report": rep(20, 4096, 8192, true, 5, 5, fdtVerified(20, now+60))})
		for i := 0; i < 6; i++ {
			now += 50
			s.add(fdtCycleStep(now+60, "serving", true))
		}
		s.add(map[string]any{"op": "copy", "id": 1, "now": now + 70, "report": rep(20, 4096, 8192, true, 6, 6, fdtVerified(20, now+70))})
		s.add(map[string]any{"op": "drain", "now": now + 80, "cycles": []any{fdtCycleStep(now+6000, "serving", false)}})
		out = append(out, fdtScriptLine(true, s))
	}

	// A drain arriving k cycles after the transfer report, its budget
	// already spent: whatever is past its cutover by then is abandoned —
	// never aborted — and named with its migration phase.
	for k := 0; k < 4; k++ {
		var s fdtScript
		s.add(map[string]any{"op": "boot", "now": t0})
		now := t0 + 5
		for i := 0; i < 8; i++ {
			s.add(fdtCycleStep(now, "serving", true))
			now += 50
		}
		s.add(map[string]any{"op": "transfer", "id": 1, "atoms": 1, "bytes": 4096, "resident": 4096, "now": now})
		for i := 0; i < k; i++ {
			now += 50
			s.add(fdtCycleStep(now, "serving", true))
		}
		s.add(map[string]any{"op": "drain", "now": now + 1, "cycles": []any{fdtCycleStep(now+9000, "serving", true)}})
		out = append(out, fdtScriptLine(false, s))
	}
	return out
}

func TestFabricDaemonControl(t *testing.T) {
	inputs := fdtControlCorpus()
	port := fdtRun(t, "daemon-control", inputs)
	golden := fdtGolden(t, "fabric_daemon_control.golden")
	if len(port) != len(golden) {
		t.Fatalf("control: port answered %d scripts, golden has %d", len(port), len(golden))
	}
	for i := range port {
		ps, gs := strings.Split(port[i], "\t"), strings.Split(golden[i], "\t")
		if len(ps) != len(gs) {
			t.Errorf("script %d: port answered %d steps, crate %d", i, len(ps), len(gs))
			continue
		}
		for j := range ps {
			if ps[j] != gs[j] {
				t.Errorf("script %d step %d:\n port %s\n rust %s", i, j, ps[j], gs[j])
				break
			}
		}
	}
}

func TestFabricDaemonControlGoldenMatchesRust(t *testing.T) {
	fdtLiveGolden(t, "daemon-control", fdtControlCorpus(), "fabric_daemon_control.golden")
}

// ---------------------------------------------------------------- admin

// fdtAdminCorpus: every route and method, the id extractor's edges, every
// reply the control loop can give, and admin.rs's #[test]s: tokens close to
// the real one (a_wrong_token_is_refused_however_close_it_is,
// an_absent_credential_never_matches_a_real_one) and the misspelt transfer
// field (a_transfer_report_refuses_a_field_it_does_not_know).
func fdtAdminCorpus(t *testing.T) []string {
	t.Helper()
	status, _, _ := strings.Cut(fdtGolden(t, "fabric_daemon_status.golden")[1], "\t")
	starting, _, _ := strings.Cut(fdtGolden(t, "fabric_daemon_status.golden")[0], "\t")
	key := [][]string{{"x-api-key", "secret"}}
	type c struct {
		method, path string
		headers      [][]string
		body         string
		reply        any
	}
	ok := map[string]any{"ok": "action-7: 1 byte(s), 1 atom(s) recorded against 'b'"}
	cases := []c{
		{"GET", "/healthz", nil, "", ok}, {"HEAD", "/healthz", nil, "", ok}, {"POST", "/healthz", nil, "", ok},
		{"GET", "/status", nil, "", ok}, {"GET", "/status", key, "", ok}, {"HEAD", "/status", key, "", ok}, {"HEAD", "/status", nil, "", ok},
		{"POST", "/status", key, "", ok}, {"GET", "/status/", key, "", ok}, {"GET", "/status?x=1", key, "", ok},
		{"GET", "/fleet", key, "", ok}, {"GET", "/placements", key, "", ok}, {"GET", "/routing", key, "", ok},
		{"GET", "/actions", key, "", ok}, {"GET", "/actions/history", key, "", ok}, {"DELETE", "/actions/history", key, "", ok},
		{"GET", "/actions/1/transfer", key, "", ok}, {"PUT", "/actions/1/abort", key, "", ok},
		{"POST", "/actions/abc/abort", nil, "", ok}, {"POST", "/actions/+5/abort", key, "", ok}, {"POST", "/actions/%35/abort", key, "", ok},
		{"POST", "/actions/%3/abort", key, "", ok}, {"POST", "/actions/-1/abort", key, "", ok}, {"POST", "/actions/+/abort", key, "", ok},
		{"POST", "/actions/18446744073709551615/abort", key, "", ok}, {"POST", "/actions/18446744073709551616/abort", key, "", ok},
		{"POST", "/actions//abort", key, "", ok}, {"POST", "/actions/007/abort", key, "", ok}, {"POST", "/actions/7/abort/", key, "", ok},
		{"POST", "/actions/history/abort", key, "", ok}, {"POST", "/actions/7", key, "", ok},
		{"POST", "/actions/7/abort", key, "", map[string]any{"err": "action-7 has already cut over: authority for shard 1 (0,0) is on 'b'"}},
		{"POST", "/actions/7/abort", key, "", "stopped"}, {"POST", "/actions/7/abort", key, "", "dropped"},
		{"POST", "/actions/7/abort", nil, "", ok},
		{"POST", "/actions/7/transfer", key, `{ "bytes_copied": 1, "atoms_coped": 1 }`, ok},
		{"POST", "/actions/7/transfer", key, `{"bytes_copied": 1, "atoms_copied": 9, "resident_bytes": 5}`, ok},
		{"POST", "/actions/7/transfer", key, `{"bytes_copied": 18446744073709551615}`, ok},
		{"POST", "/actions/7/transfer", key, `{"bytes_copied": 1, "resident_bytes": null, "atoms_copied": 0}`, ok},
		{"POST", "/actions/7/transfer", key, `{"atoms_copied": 1}`, ok}, {"POST", "/actions/7/transfer", key, ``, ok},
		{"POST", "/actions/7/transfer", key, `[1, 2]`, ok}, {"POST", "/actions/7/transfer", key, `[1]`, ok}, {"POST", "/actions/7/transfer", key, `{"bytes_copied": -1}`, ok},
		{"POST", "/actions/7/transfer", key, "{\n\"bytes_copied\": 1.5}", ok}, {"POST", "/actions/7/transfer", key, `{"bytes_copied": 1} x`, ok},
		{"POST", "/actions/7/transfer", key, `{"bytes_copied": 2}`, map[string]any{"err": "action-7 has already concluded (completed)"}},
		{"POST", "/actions/7/transfer", key, `{"bytes_copied": 2}`, "stopped"}, {"POST", "/actions/7/transfer", nil, `garbage`, ok},
		{"POST", "/actions/x/transfer", nil, `garbage`, ok}, {"PATCH", "/actions/7/transfer", key, ``, ok},
		{"GET", "/nope", nil, "", ok}, {"DELETE", "/nope", key, "", ok}, {"GET", "/", key, "", ok}, {"GET", "/STATUS", key, "", ok},
		{"GET", "/fleet", [][]string{{"X-Api-Key", "secret"}}, "", ok}, {"GET", "/fleet", [][]string{{"x-api-key", "secre"}}, "", ok},
		{"GET", "/fleet", [][]string{{"x-api-key", "secrets"}}, "", ok}, {"GET", "/fleet", [][]string{{"x-api-key", ""}}, "", ok},
		{"GET", "/fleet", [][]string{{"x-api-key", "Secret"}}, "", ok}, {"GET", "/fleet", [][]string{{"x-api-key", "secret "}}, "", ok},
		{"GET", "/fleet", [][]string{{"x-api-key", "other"}, {"x-api-key", "secret"}}, "", ok},
		{"GET", "/fleet", [][]string{{"x-api-key", "secret"}, {"x-api-key", "other"}}, "", ok},
		{"OPTIONS", "/fleet", key, "", ok},
	}
	var out []string
	line := func(token, st string, cs c) {
		if cs.headers == nil {
			cs.headers = [][]string{}
		}
		b, err := json.Marshal(map[string]any{"token": token, "status": st, "reply": cs.reply, "method": cs.method, "path": cs.path, "headers": cs.headers, "body": cs.body})
		if err != nil {
			panic(err)
		}
		out = append(out, string(b))
	}
	for _, cs := range cases {
		line("secret", status, cs)
	}
	// an_absent_credential_never_matches_a_real_one, and the snapshot
	// between binding and the first cycle.
	line("t", starting, c{"GET", "/status", nil, "", ok})
	line("t", starting, c{"GET", "/status", [][]string{{"x-api-key", "t"}}, "", ok})
	line("t", starting, c{"GET", "/routing", [][]string{{"x-api-key", "t"}}, "", ok})
	return out
}

func TestFabricDaemonAdmin(t *testing.T) {
	inputs := fdtAdminCorpus(t)
	port := fdtRun(t, "daemon-admin", inputs)
	fdtCompare(t, "admin", inputs, port, fdtGolden(t, "fabric_daemon_admin.golden"))
}

func TestFabricDaemonAdminGoldenMatchesRust(t *testing.T) {
	fdtLiveGolden(t, "daemon-admin", fdtAdminCorpus(t), "fabric_daemon_admin.golden")
}

// ---------------------------------------------------------------- the process

func fdtFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// fabricd.fct run as the service `facet exec` runs it, against two fake
// FacetQLs that record what arrives — the first half of tests/daemon.rs's
// the_daemon_boots_routes_traffic_and_follows_a_control_loop_decision, its
// own assertions: it boots (the operator port answers, and refuses without
// the token); liveness was established before the data port accepted
// anything; and a real FacetQL read arrives, byte for byte and with the
// client's own credential, at the instance the keyspace and the routing
// table name — and not at the other one. (The decision half is
// TestFabricDaemonControl's scripts: this runtime cannot make a fake
// FacetQL's /stats hot the way the crate's test injects a simulator.)
func TestFabricDaemonProcess(t *testing.T) {
	type seen struct{ method, target, key string }
	var mu sync.Mutex
	logs := map[string][]seen{}
	fake := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			logs[name] = append(logs[name], seen{r.Method, r.URL.RequestURI(), r.Header.Get("x-api-key")})
			mu.Unlock()
			if r.URL.Path == "/" {
				w.Write([]byte("FacetQL Online"))
				return
			}
			w.Write([]byte(`{"nodes":[],"next_cursor":null}`))
		}))
	}
	source, destination := fake("source"), fake("destination")
	defer source.Close()
	defer destination.Close()

	dir := t.TempDir()
	dataPort, adminPort := fdtFreePort(t), fdtFreePort(t)
	config := fmt.Sprintf(`{
		"data_listen": "127.0.0.1:%d", "admin_listen": "127.0.0.1:%d",
		"backends": [
			{"id": %q, "url": %q, "region": "us-east", "placements": [{"shard": 1, "x": 0, "y": 0}]},
			{"id": %q, "url": %q, "region": "us-west", "placements": [{"shard": 2, "x": 0, "y": 0}]}
		],
		"keyspace": {"rules": [{"kind": "Post", "address_prefix": "Post:", "shard": 1, "x": 0, "y": 0}], "fallback": {"shard": 1, "x": 0, "y": 0}},
		"cadence": {"liveness_probe_ms": 100, "telemetry_poll_ms": 100, "control_cycle_ms": 50},
		"silence_budget_ms": 5000, "probe_timeout_ms": 300,
		"policy": {"measurement_settle_ms": 0, "phase_timeout_ms": 60000},
		"drain_ms": 5000
	}`, dataPort, adminPort, fdtSource, source.URL, fdtDestination, destination.URL)
	path := dir + "/fabric.json"
	if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FABRIC_CONFIG", path)
	t.Setenv("FABRIC_ADMIN_TOKEN", "operator-secret")

	g, err := compile.File("fabricd.fct")
	if err != nil {
		t.Fatalf("compile selfhost/fabricd.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetDataDir(dir)
	var stderr strings.Builder
	var stderrMu sync.Mutex
	srv.SetStdio(strings.NewReader(""), io.Discard, fdtLockedWriter{&stderrMu, &stderr})
	go srv.RunDaemons()

	admin := fmt.Sprintf("http://127.0.0.1:%d", adminPort)
	client := &http.Client{Timeout: 5 * time.Second}
	get := func(url, key string) (int, string) {
		req, _ := http.NewRequest("GET", url, nil)
		if key != "" {
			req.Header.Set("x-api-key", key)
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// 1. it booted.
	deadline := time.Now().Add(10 * time.Second)
	for {
		code, _ := get(admin+"/healthz", "")
		if code == 200 {
			break
		}
		if time.Now().After(deadline) {
			stderrMu.Lock()
			defer stderrMu.Unlock()
			t.Fatalf("the operator port never answered; stderr: %s", stderr.String())
		}
		time.Sleep(25 * time.Millisecond)
	}
	if code, _ := get(admin+"/status", ""); code != 401 {
		t.Fatalf("/status without the token: %d", code)
	}
	code, body := get(admin+"/status", "operator-secret")
	if code != 200 {
		t.Fatalf("/status: %d %s", code, body)
	}
	var status map[string]any
	if err := json.Unmarshal([]byte(body), &status); err != nil {
		t.Fatalf("/status is not JSON: %v: %s", err, body)
	}
	backends := status["backends"].([]any)
	for i, want := range []string{"serviceable", "serviceable"} {
		if got := backends[i].(map[string]any)["availability"]; got != want {
			t.Errorf("backend %d availability %v", i, got)
		}
	}
	if got := backends[0].(map[string]any)["last_probe"]; got != "serving (200)" {
		t.Errorf("last_probe %v", got)
	}
	if got := status["placements"].([]any)[0].(map[string]any)["holder"]; got != fdtSource {
		t.Errorf("holder %v", got)
	}
	if g, _ := status["routing_generation"].(float64); g <= 0 {
		t.Errorf("routing_generation %v", status["routing_generation"])
	}

	// The operator port's questions reach the control loop and come back
	// in its words (the texts TestFabricDaemonControl checks against the
	// crate), and the loop keeps cycling on its cadence.
	req, _ := http.NewRequest("POST", admin+"/actions/99/abort", nil)
	req.Header.Set("x-api-key", "operator-secret")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	abortBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 409 || string(abortBody) != "fabricd: no action action-99\n" {
		t.Fatalf("abort of an unknown action: %d %q", resp.StatusCode, abortBody)
	}
	cycled := false
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		_, body := get(admin+"/status", "operator-secret")
		var st map[string]any
		if json.Unmarshal([]byte(body), &st) == nil {
			if c, _ := st["cycles"].(float64); c >= 3 {
				cycled = true
				break
			}
		}
	}
	if !cycled {
		t.Fatalf("the control loop is not cycling")
	}

	// 2. it routes real traffic.
	code, body = get(fmt.Sprintf("http://127.0.0.1:%d/nodes?kind=Post", dataPort), "client-secret")
	if code != 200 {
		t.Fatalf("read through the front door: %d %s", code, body)
	}
	mu.Lock()
	defer mu.Unlock()
	var arrived *seen
	for i := range logs["source"] {
		if strings.HasPrefix(logs["source"][i].target, "/nodes?") {
			arrived = &logs["source"][i]
		}
	}
	if arrived == nil || arrived.method != "GET" || arrived.target != "/nodes?kind=Post" || arrived.key != "client-secret" {
		t.Fatalf("the read did not reach the holder as sent: %+v (source saw %+v)", arrived, logs["source"])
	}
	for _, r := range logs["destination"] {
		if strings.HasPrefix(r.target, "/nodes?") {
			t.Fatalf("the read also reached an instance that does not hold the cell")
		}
	}
}

type fdtLockedWriter struct {
	mu *sync.Mutex
	w  *strings.Builder
}

func (l fdtLockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

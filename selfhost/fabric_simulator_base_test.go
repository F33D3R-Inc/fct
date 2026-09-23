package selfhost

import (
	"net/http/httptest"
	"testing"

	"facet/internal/compile"
	"facet/runtime"
)

// fabric_simulator_base.fct ports fabric-simulator's clock.rs, fault.rs,
// rng.rs and workload.rs. Every expected value below was printed by the real
// crate: a scratch binary linking fabric-simulator, fabric-controller and
// fabric-core by path ran exactly the inputs the demo procs use. u64 values
// are compared as their i64 bit pattern; floats as round(v*10^k) integers
// (Rust's f64::round and this runtime's round() are both
// half-away-from-zero).

func simBaseLoadApp(t *testing.T) *httptest.Server {
	t.Helper()
	g, err := compile.File("fabric_simulator_base.fct")
	if err != nil {
		t.Fatalf("compile selfhost/fabric_simulator_base.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestFabricSimulatorBase(t *testing.T) {
	ts := simBaseLoadApp(t)
	d := postJSON(t, ts, "runFabricSimulatorBaseDemo")

	want := []struct{ field, value string }{
		// SimClock::new(1000, 250): start, tick, ticks; three advance() instants; start/tick/ticks/now after them; SimClock::new(0, 0) tick and first advance.
		{"demoSimClockResult", "1000|250|0|1250|1500|1750|1000|250|3|1750|1|1"},
		// label()=Display for every variant; HotShard multipliers 0.25, 2.25 and 0.05 pin `{:.1}`'s round-half-even on the exact binary value.
		{"demoFaultResult", "node-loss=node 'n1' lost;node-recovery=node 'n1' recovered;partition=region 'us-east' partitioned from the Fabric;heal=region 'us-west' rejoined the Fabric;hot-shard=shard 3 (7,2) running 2.5x for 4000ms;hot-shard=shard 1 (0,0) running 6.0x for 20000ms;hot-shard=shard 12 (11,12) running 0.2x for 1ms;hot-shard=shard 12 (11,12) running 2.2x for 0ms;hot-shard=shard 1 (0,0) running 0.1x for 1500ms;hot-shard=shard 3 (7,2) running 1234.6x for 400000ms"},
		// Rng::new(42), five next_u64() draws as i64 (the fifth has its top bit set).
		{"demoRngSeed42SequenceResult", "2949826092126892291|5139283748462763858|6349198060258255764|701532786141963250|-2430762948046562554"},
		// the_same_seed_gives_the_same_sequence (1000 draws) plus the wrapping sum of the draws.
		{"demoRngSameSeedSameSequenceResult", "true|6764756057421943536"},
		// different_seeds_diverge_immediately plus both first draws.
		{"demoRngDifferentSeedsDivergeResult", "true|-1612297016619662647|-4627371582388691390"},
		// Rng::new(7): first five next_f64()*1e15, floats_stay_in_the_unit_interval over the next 10000, their sum*1e6.
		{"demoRngFloatsStayInUnitIntervalResult", "923700106930456|995512885175621|436030405349057|148681343155416|31936558057150|true|5018080082"},
		// seed 42 after four draws: below(1000), below(0) (draws nothing), next draw; then Rng::new(3).below(n) over a spread of n.
		{"demoRngBelowResult", "62|0|-2430762948046562554|0|0|2|5|8|69|533|861054|2339306488|376737"},
		// Rng::new(99): range(10,20), range(-5,5), range(0.5,1.5), each *1e12.
		{"demoRngRangeResult", "18117024988543|312810616105|1408024941611"},
		// Rng::new(5): chance(0.5), chance(0.6) from the same state, and that state's next_f64()*1e15.
		{"demoRngChanceResult", "false|true|557548019203296"},
		// Rng::new(42) state, then fork(): parent state, child state, child first draw, parent next draw.
		{"demoRngForkResult", "-7046029254386353089|4354685564936845396|-5271293442898752234|5113007443819495584|5139283748462763858"},
		// class_profile for all seven classes: base_ops, read_ratio*1e6, read/write latency, bytes_per_op, write_ratio()*1e6.
		{"demoClassProfileResult", "ReadHeavy:800:920000:400:1200:512:80000;WriteHeavy:400:150000:900:2500:1024:850000;Mixed:500:550000:600:1500:768:450000;EventHeavy:1200:350000:300:800:256:650000;Media:150:850000:2500:9000:65536:150000;Realtime:2000:600000:150:400:128:400000;Unknown:100:500000:1000:1000:512:500000"},
		// diurnal() at trough, peak, mid-rise, offset wrap, zero period, and three simulator instants, each *1e12.
		{"demoDiurnalResult", "600000000000|1400000000000|1000000000000|600000000000|600000000000|961885333333|1367229333333|893805333333"},
	}
	for _, w := range want {
		got, _ := d[w.field].(string)
		if got != w.value {
			t.Errorf("%s:\n got: %s\nwant: %s", w.field, got, w.value)
		}
	}
}

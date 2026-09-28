package integration

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"facet/internal/compile"
	"facet/runtime"
)

// beatWav is a 44.1 kHz stereo 16-bit WAV of secs10/10 seconds: two sawtooth
// voices, loud for the first 0.1 s of every half second (120 BPM) and quiet
// between. Integer arithmetic only, so the bytes are the same wherever they
// are built — the reference values below are zior-engine's own decoder.rs +
// features.rs run on exactly these bytes.
func beatWav(secs10 int) []byte {
	const r = 44100
	n := r * secs10 / 10
	var d bytes.Buffer
	for i := 0; i < n; i++ {
		var l, rr int
		if i%22050 < 4410 {
			l, rr = (i*523)%441*120-26400, (i*330)%441*100-22000
		} else {
			l, rr = (i*523)%441*30-6600, (i*330)%441*25-5500
		}
		binary.Write(&d, binary.LittleEndian, int16(l))
		binary.Write(&d, binary.LittleEndian, int16(rr))
	}
	var out bytes.Buffer
	out.WriteString("RIFF")
	binary.Write(&out, binary.LittleEndian, uint32(36+d.Len()))
	out.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(2), uint32(r), uint32(r * 4), uint16(4), uint16(16)} {
		binary.Write(&out, binary.LittleEndian, v)
	}
	out.WriteString("data")
	binary.Write(&out, binary.LittleEndian, uint32(d.Len()))
	out.Write(d.Bytes())
	return out.Bytes()
}

// verbatimFlac re-encodes a 44.1 kHz stereo 16-bit WAV's samples as FLAC:
// fixed 4096-sample blocks (the last shorter, its size in the 16-bit field),
// independent channels, VERBATIM subframes, CRC-8 headers and CRC-16 frames.
// keyOf is the upload reply's signal_preview.key.
func keyOf(up map[string]any) any {
	p, _ := up["signal_preview"].(map[string]any)
	return p["key"]
}

func verbatimFlac(wav []byte) []byte {
	pcm := wav[44:]
	n := len(pcm) / 4
	crc := func(b []byte, poly, width uint) uint {
		c, top, mask := uint(0), uint(1)<<(width-1), uint(1)<<width-1
		for _, x := range b {
			c ^= uint(x) << (width - 8)
			for k := 0; k < 8; k++ {
				if c&top != 0 {
					c = (c<<1 ^ poly) & mask
				} else {
					c = c << 1 & mask
				}
			}
		}
		return c
	}
	var out bytes.Buffer
	out.WriteString("fLaC")
	// STREAMINFO: block sizes, frame sizes unknown, 44100 Hz, 2 ch, 16 bits, n samples, no MD5
	si := make([]byte, 34)
	binary.BigEndian.PutUint16(si[0:], 4096)
	binary.BigEndian.PutUint16(si[2:], 4096)
	binary.BigEndian.PutUint64(si[10:], uint64(44100)<<44|uint64(1)<<41|uint64(15)<<36|uint64(n))
	out.Write([]byte{0x80, 0, 0, 34})
	out.Write(si)
	for f, pos := 0, 0; pos < n; f, pos = f+1, pos+4096 {
		m := min(4096, n-pos)
		fr := []byte{0xff, 0xf8, 0x79, 0x18, byte(f), byte((m - 1) >> 8), byte(m - 1)}
		fr = append(fr, byte(crc(fr, 7, 8)))
		for c := 0; c < 2; c++ {
			fr = append(fr, 0x02) // padding 0, type VERBATIM, no wasted bits
			for i := pos; i < pos+m; i++ {
				fr = append(fr, pcm[4*i+2*c+1], pcm[4*i+2*c])
			}
		}
		c16 := crc(fr, 0x8005, 16)
		out.Write(append(fr, byte(c16>>8), byte(c16)))
	}
	return out.Bytes()
}

// facets/media/ziorservice.fct (zior.fct over audio.fct) as zior-engine's POST /upload: a multipart `audio`
// file is stored, read back by the proc (uploadBytes), decoded, downsampled
// 2:1, fingerprinted and measured in fct — the fingerprint byte for byte
// and the features to f32 precision against the reference; a clip
// under five seconds and a file that is neither WAV nor FLAC are 422; the
// same audio uploaded as FLAC stores the same analysis.
func TestAudioUploadMatchesZiorEngine(t *testing.T) {
	t.Setenv("FACET_UPLOAD_DIR", t.TempDir())
	t.Setenv("FACET_LOG_LEVEL", "error")
	// A host importing the library: the member policy and a sign-in.
	dir := t.TempDir()
	lib, _ := filepath.Abs(filepath.Join("..", "..", "facets", "media", "ziorservice.fct"))
	rel, err := filepath.Rel(dir, lib)
	if err != nil {
		t.Fatal(err)
	}
	host := filepath.Join(dir, "host.fct")
	if err := os.WriteFile(host, []byte(`import "`+filepath.ToSlash(rel)+`"
app ZiorHost:
    type TokenDTO:
        token: text
    policy member:
        actor != "guest"
    action signin(handle: text) -> TokenDTO:
        establish actor handle
        return TokenDTO{token: sessionToken}
    api POST "/api/sessions" -> signin status 201
    view V at "/":
        text "x"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := compile.File(host)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/sessions", "application/json", strings.NewReader(`{"handle":"ada"}`))
	if err != nil {
		t.Fatal(err)
	}
	var tok struct{ Token string }
	json.NewDecoder(resp.Body).Decode(&tok)
	resp.Body.Close()
	if tok.Token == "" {
		t.Fatalf("sign-in = %d, no token", resp.StatusCode)
	}

	upload := func(track string, audio []byte) (int, string) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		if track != "" {
			mw.WriteField("track_id", track)
		}
		part, _ := mw.CreateFormFile("audio", "track.wav")
		part.Write(audio)
		mw.Close()
		req, _ := http.NewRequest("POST", ts.URL+"/api/zior/upload", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Authorization", "Bearer "+tok.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}

	// Unauthenticated: refused before anything is stored or decoded.
	var anon bytes.Buffer
	amw := multipart.NewWriter(&anon)
	amw.WriteField("track_id", "anon")
	ap, _ := amw.CreateFormFile("audio", "anon.wav")
	ap.Write(beatWav(55))
	amw.Close()
	if r, err := http.Post(ts.URL+"/api/zior/upload", amw.FormDataContentType(), &anon); err != nil || r.StatusCode != 401 {
		t.Errorf("anonymous upload = %v %v, want 401", r.StatusCode, err)
	}
	// No track_id: the id is the fingerprint's, and the fingerprint is
	// zior-engine's own (fingerprint.rs over the same bytes).
	const fp = "fb3104b1fe0508293c36b9a944bb3b1ef1d4704bbf1a68cd01b3195a4a5a2356"
	code, body := upload("", beatWav(55))
	if code != 201 {
		t.Fatalf("upload = %d: %s", code, body)
	}
	var up map[string]any
	json.Unmarshal([]byte(body), &up)
	if up["track_id"] != "zior_"+fp[:16] || up["fingerprint"] != fp || up["status"] != "processed" || keyOf(up) != "F major" {
		t.Errorf("upload reply = %s", body)
	}
	if code, body := upload("short", beatWav(30)); code != 422 {
		t.Errorf("a 3 s clip = %d, want 422: %s", code, body)
	}
	if code, body := upload("mp3", []byte("ID3\x04\x00\x00\x00\x00\x00\x00 not a wave")); code != 422 {
		t.Errorf("a non-WAV file = %d, want 422: %s", code, body)
	}

	// The same audio as FLAC: decoded in fct (flac.fct), it analyses exactly
	// as the WAV did — zior-engine's own FLAC path gives the same numbers.
	code, body = upload("beat.flac", verbatimFlac(beatWav(55)))
	up = nil
	json.Unmarshal([]byte(body), &up)
	if code != 201 || up["fingerprint"] != fp || keyOf(up) != "F major" {
		t.Errorf("FLAC upload = %d: %s", code, body)
	}

	rows := srv.EntityRows("TrackAnalysis")
	if len(rows) != 2 {
		t.Fatalf("%d analyses stored, want 2", len(rows))
	}
	row := rows[0].(map[string]any)
	if flac := rows[1].(map[string]any); flac["track"] != "beat.flac" {
		t.Errorf("second row = %v", flac)
	} else {
		for k, v := range row {
			if k != "id" && k != "track" && k != "created" && flac[k] != v {
				t.Errorf("FLAC %s = %v, WAV %v", k, flac[k], v)
			}
		}
	}
	f := func(k string) float64 {
		switch v := row[k].(type) {
		case float64:
			return v
		case int:
			return float64(v)
		case int64:
			return float64(v)
		}
		t.Fatalf("%s = %T %v", k, row[k], row[k])
		return 0
	}
	if row["owner"] != "ada" || row["track"] != "zior_"+fp[:16] || row["landmarks"] != 1410 || f("sample_rate") != 44100 || f("channels") != 2 || f("key") != 5 || row["is_major"] != true {
		t.Errorf("row = %v", row)
	}
	// zior-engine on the same bytes (f32 arithmetic there, f64 here).
	for k, want := range map[string]float64{
		"duration_secs": 5.5, "bpm_raw": 120.27273, "bpm": 0.4459596, "tonal_valence": 0.67,
		"rms_energy": 0.10679102, "dynamic_range": 0.05056685, "spectral_centroid": 0.61102563,
		"spectral_rolloff": 0.97443336, "spectral_flux": 0.09516824, "bass_energy": 0.0012917314,
		"mid_energy": 0.0609868, "treble_energy": 0.93781465, "vocal_probability": 0.07465938,
	} {
		if got := f(k); math.Abs(got-want) > 1e-4*math.Max(1, math.Abs(want)) {
			t.Errorf("%s = %v, zior-engine %v", k, got, want)
		}
	}

	// The rest of the service over HTTP: the signal (zior-engine's axes,
	// mood, tags and descriptor for these bytes), events and the emitter's
	// behavioural update, similar tracks and clusters.
	get := func(path string) map[string]any {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m)
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s = %d %v", path, resp.StatusCode, m)
		}
		return m
	}
	track := "zior_" + fp[:16]
	sig := get("/api/zior/signal/" + track)
	if sig["mood"] != "nostalgic" || sig["descriptor"] != "nostalgic · F major · 120 BPM · instrumental" || sig["creator_id"] != "unknown" || sig["early_retention"] != 0.5 {
		t.Errorf("signal = %v", sig)
	}
	if tags, _ := sig["context_tags"].([]any); len(tags) != 1 || tags[0] != "memories" {
		t.Errorf("context_tags = %v", sig["context_tags"])
	}
	tv, _ := sig["topic_vector"].([]any)
	for i, want := range []float64{0.49030986, 0.08788256, 0.2114678, 0.5921077, 0.15614957, 0.20609212, 0.236907, 0.48330233} {
		if len(tv) != 8 || math.Abs(tv[i].(float64)-want) > 1e-4 {
			t.Fatalf("topic_vector = %v, zior-engine axis %d %v", tv, i, want)
		}
	}
	events := `{"events":[{"track_id":"` + track + `","user_id":"u1","event_type":"play","position_secs":0,"duration_secs":5.5,"session_id":"s"},` +
		`{"track_id":"` + track + `","user_id":"u1","event_type":"complete","position_secs":5.5,"duration_secs":5.5,"session_id":"s"},` +
		`{"track_id":"` + track + `","user_id":"u2","event_type":"play","position_secs":0,"duration_secs":5.5,"session_id":"t"},` +
		`{"track_id":"elsewhere","user_id":"u2","event_type":"share","position_secs":0,"duration_secs":5.5,"session_id":"t"}]}`
	resp, err = http.Post(ts.URL+"/api/zior/events", "application/json", strings.NewReader(events))
	if err != nil {
		t.Fatal(err)
	}
	var ing map[string]any
	json.NewDecoder(resp.Body).Decode(&ing)
	resp.Body.Close()
	if ids, _ := ing["track_ids"].([]any); ing["ingested"] != 4.0 || len(ids) != 2 || ids[0] != track || ids[1] != "elsewhere" {
		t.Errorf("events = %d %v", resp.StatusCode, ing)
	}
	sig = get("/api/zior/signal/" + track)
	if sig["completion_rate"] != 0.5 || sig["exposure_count"] != 2.0 || sig["velocity_score"].(float64) < 0.88 {
		t.Errorf("after events: completion %v exposure %v velocity %v", sig["completion_rate"], sig["exposure_count"], sig["velocity_score"])
	}
	sim := get("/api/zior/similar/" + track)
	if list, _ := sim["similar"].([]any); len(list) != 1 || list[0].(map[string]any)["track_id"] != "beat.flac" || math.Abs(list[0].(map[string]any)["similarity"].(float64)-1) > 1e-9 {
		t.Errorf("similar = %v", sim)
	}
	cl := get("/api/zior/clusters")
	if cs, _ := cl["clusters"].([]any); cl["n_clusters"] != 1.0 || len(cs) != 1 || cs[0].(map[string]any)["track_count"] != 2.0 {
		t.Errorf("clusters = %v", cl)
	}
}

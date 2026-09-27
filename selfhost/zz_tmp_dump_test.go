package selfhost

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"facet/internal/compile"
)

// TEMP (remove): dump both compilers' IR for FCT_DUMP files into FCT_DUMP_DIR.
func TestTmpDump(t *testing.T) {
	files := strings.Fields(os.Getenv("FCT_DUMP"))
	if len(files) == 0 {
		t.Skip()
	}
	dir := os.Getenv("FCT_DUMP_DIR")
	ts, root := loadDriverFileApp(t)
	for _, f := range files {
		base := strings.ReplaceAll(strings.TrimSuffix(f, ".fct"), "/", "_")
		if g, err := compile.File(f); err == nil {
			b, _ := json.MarshalIndent(g, "", " ")
			os.WriteFile(filepath.Join(dir, base+".go.json"), b, 0o644)
		}
		got, gerr := driverGot(t, ts, root, f)
		if gerr != "" {
			os.WriteFile(filepath.Join(dir, base+".fct.err"), []byte(gerr), 0o644)
			continue
		}
		b, _ := json.MarshalIndent(got, "", " ")
		os.WriteFile(filepath.Join(dir, base+".fct.json"), b, 0o644)
	}
}

package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/F33D3R-Inc/fct/internal/parser"
)

// runLSP is a minimal Language Server (JSON-RPC over stdio) that publishes FDL
// syntax diagnostics as you type. Wire any LSP-capable editor to run `fct lsp`
// for the `fct`/`.fct` language. It reuses the real compiler, so editor errors
// match `fct check`.
func runLSP() error {
	r := bufio.NewReader(os.Stdin)
	w := bufio.NewWriter(os.Stdout)
	for {
		body, err := readLSPMessage(r)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(body, &msg) != nil {
			continue
		}
		switch msg.Method {
		case "initialize":
			writeLSP(w, lspResult(msg.ID, map[string]any{
				"capabilities": map[string]any{"textDocumentSync": 1}, // full sync
				"serverInfo":   map[string]any{"name": "fct-lsp", "version": version},
			}))
		case "shutdown":
			writeLSP(w, lspResult(msg.ID, nil))
		case "exit":
			return nil
		case "textDocument/didOpen", "textDocument/didChange", "textDocument/didSave":
			uri, text := extractDoc(msg.Method, msg.Params)
			if uri != "" {
				publishDiagnostics(w, uri, text)
			}
		}
	}
}

func readLSPMessage(r *bufio.Reader) ([]byte, error) {
	length := 0
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break // end of headers
		}
		if v, ok := strings.CutPrefix(line, "Content-Length:"); ok {
			length, _ = strconv.Atoi(strings.TrimSpace(v))
		}
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func writeLSP(w *bufio.Writer, v any) {
	data, _ := json.Marshal(v)
	w.WriteString("Content-Length: " + strconv.Itoa(len(data)) + "\r\n\r\n")
	w.Write(data)
	w.Flush()
}

func lspResult(id json.RawMessage, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func extractDoc(method string, params json.RawMessage) (uri, text string) {
	var p struct {
		TextDocument struct {
			URI  string `json:"uri"`
			Text string `json:"text"`
		} `json:"textDocument"`
		ContentChanges []struct {
			Text string `json:"text"`
		} `json:"contentChanges"`
	}
	if json.Unmarshal(params, &p) != nil {
		return "", ""
	}
	uri = p.TextDocument.URI
	if method == "textDocument/didChange" && len(p.ContentChanges) > 0 {
		return uri, p.ContentChanges[len(p.ContentChanges)-1].Text
	}
	if p.TextDocument.Text != "" {
		return uri, p.TextDocument.Text
	}
	// didSave without text → read from disk
	if path, ok := strings.CutPrefix(uri, "file://"); ok {
		if data, err := os.ReadFile(path); err == nil {
			return uri, string(data)
		}
	}
	return uri, ""
}

func publishDiagnostics(w *bufio.Writer, uri, text string) {
	diags := []map[string]any{}
	if _, err := parser.Parse(text); err != nil {
		line, col, msg := splitPos(err.Error())
		diags = append(diags, map[string]any{
			"range": map[string]any{
				"start": map[string]any{"line": line, "character": col},
				"end":   map[string]any{"line": line, "character": col + 1},
			},
			"severity": 1, // error
			"source":   "fct",
			"message":  msg,
		})
	}
	writeLSP(w, map[string]any{
		"jsonrpc": "2.0",
		"method":  "textDocument/publishDiagnostics",
		"params":  map[string]any{"uri": uri, "diagnostics": diags},
	})
}

// splitPos parses a "line:col: message" compiler error into 0-based positions.
func splitPos(s string) (line, col int, msg string) {
	parts := strings.SplitN(s, ": ", 2)
	msg = s
	if len(parts) == 2 {
		msg = parts[1]
		lc := strings.SplitN(parts[0], ":", 2)
		if len(lc) == 2 {
			l, e1 := strconv.Atoi(lc[0])
			c, e2 := strconv.Atoi(lc[1])
			if e1 == nil && e2 == nil {
				return max0(l - 1), max0(c - 1), msg
			}
		}
	}
	return 0, 0, msg
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

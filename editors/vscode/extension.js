// Minimal Facet language-client for VS Code. It launches `facet lsp` and speaks
// LSP to it over stdio directly — no third-party npm dependency — wiring live
// diagnostics, completion, hover, and go-to-definition into the editor. Syntax
// highlighting comes from the bundled TextMate grammar and needs no server.
const vscode = require("vscode");
const cp = require("child_process");

let proc = null;
let seq = 0;
const pending = new Map();
let diagnostics = null;

function activate(context) {
  const bin = vscode.workspace.getConfiguration("facet").get("serverPath", "facet");
  diagnostics = vscode.languages.createDiagnosticCollection("facet");
  context.subscriptions.push(diagnostics);

  try {
    proc = cp.spawn(bin, ["lsp"], { stdio: ["pipe", "pipe", "inherit"] });
  } catch (e) {
    vscode.window.showErrorMessage("Facet: could not start `" + bin + " lsp`: " + e.message);
    return;
  }
  proc.on("error", (e) => vscode.window.showErrorMessage("Facet LSP: " + e.message));

  readLoop(proc.stdout);

  request("initialize", { processId: process.pid, rootUri: null, capabilities: {} }).then(() => {
    notify("initialized", {});
    for (const doc of vscode.workspace.textDocuments) if (doc.languageId === "facet") didOpen(doc);
  });

  context.subscriptions.push(
    vscode.workspace.onDidOpenTextDocument((d) => d.languageId === "facet" && didOpen(d)),
    vscode.workspace.onDidChangeTextDocument((e) => e.document.languageId === "facet" && didChange(e.document)),
    vscode.workspace.onDidCloseTextDocument((d) => d.languageId === "facet" && notify("textDocument/didClose", { textDocument: { uri: d.uri.toString() } }))
  );

  context.subscriptions.push(
    vscode.languages.registerCompletionItemProvider("facet", {
      provideCompletionItems: (doc, pos) =>
        request("textDocument/completion", posParams(doc, pos)).then((r) => {
          const items = (r && r.items) || [];
          return items.map((it) => {
            const ci = new vscode.CompletionItem(it.label);
            if (it.detail) ci.detail = it.detail;
            return ci;
          });
        }),
    }, " ", "."),
    vscode.languages.registerHoverProvider("facet", {
      provideHover: (doc, pos) =>
        request("textDocument/hover", posParams(doc, pos)).then((r) => {
          if (!r || !r.contents) return null;
          return new vscode.Hover(r.contents.value || r.contents);
        }),
    }),
    vscode.languages.registerDefinitionProvider("facet", {
      provideDefinition: (doc, pos) =>
        request("textDocument/definition", posParams(doc, pos)).then((r) => {
          if (!r || !r.range) return null;
          return new vscode.Location(vscode.Uri.parse(r.uri), toRange(r.range));
        }),
    })
  );
}

function posParams(doc, pos) {
  return { textDocument: { uri: doc.uri.toString() }, position: { line: pos.line, character: pos.character } };
}
function toRange(r) {
  return new vscode.Range(r.start.line, r.start.character, r.end.line, r.end.character);
}

function didOpen(doc) {
  notify("textDocument/didOpen", { textDocument: { uri: doc.uri.toString(), languageId: "facet", version: doc.version, text: doc.getText() } });
}
function didChange(doc) {
  notify("textDocument/didChange", { textDocument: { uri: doc.uri.toString(), version: doc.version }, contentChanges: [{ text: doc.getText() }] });
}

function request(method, params) {
  const id = ++seq;
  send({ jsonrpc: "2.0", id, method, params });
  return new Promise((resolve) => pending.set(id, resolve));
}
function notify(method, params) {
  send({ jsonrpc: "2.0", method, params });
}
function send(msg) {
  if (!proc || !proc.stdin.writable) return;
  const body = Buffer.from(JSON.stringify(msg), "utf8");
  proc.stdin.write("Content-Length: " + body.length + "\r\n\r\n");
  proc.stdin.write(body);
}

function readLoop(stream) {
  let buf = Buffer.alloc(0);
  stream.on("data", (chunk) => {
    buf = Buffer.concat([buf, chunk]);
    for (;;) {
      const sep = buf.indexOf("\r\n\r\n");
      if (sep < 0) return;
      const header = buf.slice(0, sep).toString("utf8");
      const m = /Content-Length:\s*(\d+)/i.exec(header);
      if (!m) { buf = buf.slice(sep + 4); continue; }
      const len = parseInt(m[1], 10);
      if (buf.length < sep + 4 + len) return;
      const body = buf.slice(sep + 4, sep + 4 + len).toString("utf8");
      buf = buf.slice(sep + 4 + len);
      try { dispatch(JSON.parse(body)); } catch (_) {}
    }
  });
}

function dispatch(msg) {
  if (msg.id !== undefined && pending.has(msg.id)) {
    pending.get(msg.id)(msg.result);
    pending.delete(msg.id);
    return;
  }
  if (msg.method === "textDocument/publishDiagnostics") {
    const p = msg.params;
    const items = (p.diagnostics || []).map((d) => {
      const r = toRange(d.range);
      const sev = d.severity === 1 ? vscode.DiagnosticSeverity.Error : vscode.DiagnosticSeverity.Warning;
      const diag = new vscode.Diagnostic(r, d.message, sev);
      diag.source = d.source || "facet";
      return diag;
    });
    diagnostics.set(vscode.Uri.parse(p.uri), items);
  }
}

function deactivate() {
  if (proc) proc.kill();
}

module.exports = { activate, deactivate };

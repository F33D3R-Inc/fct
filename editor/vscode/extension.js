// VS Code client for the Facet Architecture LSP. Highlighting works from the
// grammar with no setup; this adds live diagnostics by launching `fct lsp`.
//
//   npm install && npx vsce package   # build a .vsix, or press F5 to debug
const { workspace } = require('vscode');
const { LanguageClient, TransportKind } = require('vscode-languageclient/node');

let client;

function activate(context) {
  const cmd = workspace.getConfiguration('fct').get('serverPath') || 'fct';
  const serverOptions = {
    run: { command: cmd, args: ['lsp'], transport: TransportKind.stdio },
    debug: { command: cmd, args: ['lsp'], transport: TransportKind.stdio },
  };
  const clientOptions = {
    documentSelector: [{ scheme: 'file', language: 'fct' }],
  };
  client = new LanguageClient('fct', 'Facet Architecture', serverOptions, clientOptions);
  client.start();
}

function deactivate() {
  return client ? client.stop() : undefined;
}

module.exports = { activate, deactivate };

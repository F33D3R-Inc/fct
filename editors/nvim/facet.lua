-- Neovim integration for Facet: register the .fct filetype and start the
-- `facet lsp` language server (diagnostics, completion, hover, go-to-definition).
-- Source this file from your config (e.g. `require("facet")` after putting it on
-- the runtimepath), or copy the contents into init.lua.
--
-- Syntax highlighting is provided separately by editors/vim/ (put it on your
-- runtimepath); this file wires the language server.

local M = {}

function M.setup(opts)
  opts = opts or {}
  local cmd = opts.cmd or { "facet", "lsp" }

  -- .fct -> filetype facet
  vim.filetype.add({ extension = { fct = "facet" } })

  -- Start (or reuse) the language server for every Facet buffer.
  vim.api.nvim_create_autocmd("FileType", {
    pattern = "facet",
    callback = function(args)
      vim.lsp.start({
        name = "facet-lsp",
        cmd = cmd,
        root_dir = vim.fs.dirname(vim.api.nvim_buf_get_name(args.buf)),
      })
    end,
  })
end

return M

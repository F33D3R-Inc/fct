" Vim syntax file for the Facet application language.
" Drop editors/vim/ on your runtimepath (it provides ftdetect + syntax).
if exists("b:current_syntax")
  finish
endif

syn keyword facetDeclaration app auth entity enum state derive policy action job component layout theme view
syn keyword facetControl     for in where by limit if requires check on start every at desc asc
syn keyword facetNode        box text button input select option form upload use slot link add set remove clear bind placeholder label
syn keyword facetType        int text bool money date
syn keyword facetConstant    true false actor role verified
syn keyword facetBuiltin     now rand count sum abs min max floor round money len upper lower trim year month day

syn match   facetAnnotation  "@\(client\|server\|secret\|optimistic\)\>"
syn match   facetNumber      "\<[0-9]\+\>"
syn match   facetOperator    "->\|=>\|==\|!=\|<=\|>=\|&&\|||\|[-+*/%<>=!]"
syn match   facetTypeName    "\<[A-Z][A-Za-z0-9_]*\>"
syn match   facetComment     "#.*$" contains=@Spell

syn region  facetString      start=+"+ skip=+\\"+ end=+"+ contains=facetInterp
syn region  facetInterp      matchgroup=facetInterpDelim start="{" end="}" contained contains=facetBuiltin,facetConstant,facetNumber,facetTypeName,facetOperator

hi def link facetDeclaration  Keyword
hi def link facetControl      Statement
hi def link facetNode         Function
hi def link facetType         Type
hi def link facetConstant     Constant
hi def link facetBuiltin      Identifier
hi def link facetAnnotation   PreProc
hi def link facetNumber       Number
hi def link facetOperator     Operator
hi def link facetTypeName     Structure
hi def link facetComment      Comment
hi def link facetString       String
hi def link facetInterpDelim  Special

let b:current_syntax = "facet"

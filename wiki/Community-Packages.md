# Community Packages & Contributing Facets

Facets are shareable. There are two paths: publish a **package** anyone can
install, or submit your facet for inclusion in the **standard library**. Both
run through GitHub — this is also where to ask questions and get help.

## Getting help

- **Questions / "how do I…"** — open a
  [Discussion](https://github.com/F33D3R-Inc/fct/discussions) (or an issue if
  Discussions isn't enabled yet). Check
  [Troubleshooting](Troubleshooting.md) first — most common errors are there.
- **Bugs** — open an [issue](https://github.com/F33D3R-Inc/fct/issues) with
  your `fct version`, the `.fct` source if relevant, and the exact error
  (compiler errors name the fix — paste them whole).
- **Security problems** — never in public; see
  [SECURITY.md](https://github.com/F33D3R-Inc/fct/blob/main/SECURITY.md).

## Publish a package

A package is a manifest plus `.fct` files, built and shared like npm packages
— except the registry **refuses anything that doesn't compile**:

```sh
fct init my-pkg social/post-card   # scaffold (fct.pkg.json + .fct files)
# …write your facets…
fct check my-pkg                   # validate locally
fct pack my-pkg                    # build a .tgz (validates it compiles)
fct publish my-pkg                 # submit to the registry (FA_REGISTRY)
```

Consumers install with:

```sh
fct search post
fct add social/post-card           # into facets/ — validated again on install
```

`fct add` also accepts a URL or local path, and `fct registry ./store`
self-hosts a registry.

## Submit a facet to the standard library

The std library (229 facets) grows by curation. If you've built something
general-purpose — a component pattern others will reuse — submit it:

1. **Fork** the repo and add your facet to the right category file in
   `std/facets/` (e.g. `overlays.fct`, `commerce.fct`), following the house
   style: `fa-`-prefixed CSS classes, `data-action` events named
   `noun.verb`, props declared in `what:`, per-instance ids where surgical
   updates matter.
2. Add its CSS to `std/style.css` under the matching section, responsive
   like its neighbors.
3. Run `go test ./std` — every stdlib facet must compile (the test enforces
   it).
4. Open a **pull request** titled `std: add <FacetName>` describing what it's
   for and which product surface it covers. Include the rendered HTML of a
   sample render if you can.

What gets accepted: components that are generic (no app-specific logic),
accessible (real `<button>`s, labels, alt text), and not duplicates of an
existing facet (extend the existing one instead). What doesn't: one-off
app components — publish those as a package instead.

## Contributing to the framework itself

Compiler, runtime, `fa` library: see
[CONTRIBUTING.md](https://github.com/F33D3R-Inc/fct/blob/main/CONTRIBUTING.md)
and the ADR log in
[DECISIONS.md](https://github.com/F33D3R-Inc/fct/blob/main/DECISIONS.md) —
read the relevant ADRs before proposing a direction change.

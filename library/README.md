# Facet standard library — `github.com/F33D3R-Inc/facets`

The reusable facets f33d3r.com (and any Facet app) is assembled from — imports, not
hand-written screens. Built entirely on the v1.16 language surface; published via
the registry. Versioned independently of the language (`facet >= 1.16.0`).

> **v0.1.0 — the f33d3r core batch.** Value-first, not count-first: this batch is the
> ~20 facets f33d3r's home + profile actually need, and a full exercise of the
> language (filtered `count`/`exists`, `tabs`, `match`, `richtext`/`video`,
> `pending`/`failed`, `contains`/search, `remove … where`, components, `image`/`badge`,
> `icon`). The 250+ destination grows category by category from here (see ROADMAP §3).

## Import

```
import "github.com/F33D3R-Inc/facets/social/postcard.fct"
```

Locally (this repo) the same files live under `library/` and build with `facet build`.

## What's in v0.1.0

| Category | Facets |
|---|---|
| `ui/` | Avatar · VerifiedBadge · UserChip · Trend · Trends (ui) · Nav (icon rail, ui) |
| `social/` | PostCard · EngagementBar · ComposeBox · FollowButton · WhoToFollow |
| `forms/` | SearchBox |
| `notify/` | UnreadBadge · NotificationItem |
| `profile/` | ProfileHeader |
| `data/` | Feed (a full vertical slice: entities + actions + policy + content) |
| `wireframes/` | Shell (the 3-column app skeleton) |
| — | f33d3r (playground baseplate) |

## Two composition tracks, one set of atoms

The same component atoms serve both ways an app is built:

- **Plain-app track** — `library/home.fct` (`app F33D3RHome`) imports the atoms and
  assembles a home screen directly.
- **Layered (typed-brick) track** — `library/f33d3r.fct` (`playground`) → `Shell`
  (`wireframe`, typed sockets) → `Nav`/`Trends` (`ui`) + `Feed` (`data`). The `data`
  facet imports the **same** `PostCard`/`ComposeBox`/`SearchBox`/`WhoToFollow` atom
  files — a layered build can pull in component-only modules (closed in v1.17.0).

```
facet dev library/home.fct      # plain-app track
facet dev library/f33d3r.fct    # layered typed-brick track
```

## Quality bar (per facet)

- Compiles (`facet build`); runs where it makes sense (`facet dev`).
- A doc comment at the top of the file.
- Placement-sound by construction — the compiler enforces it across the import
  boundary (an imported server action can't read your `@client` state).

# The Facet Library — plan (item 6, the v1.17 milestone)

The old project shipped **250+ reusable facets**; the restarted FA has the
*mechanism* (the registry, layered facets, `component`/`layout`) but none of the
*content*. This is the plan to (re)build the library **on top of the now-complete
language (v1.16)** and publish it via the registry, so f33d3r.com — and any app —
is assembled from imports, not written from scratch.

Principle: **value-first, not count-first.** 250+ is the destination; the first
batch (the ~18 facets f33d3r actually needs) is the deliverable that matters. The
count grows by category afterward.

---

## 1. What a "library facet" is

A publishable module built from the v1.16 language surface, imported via the
registry (`import "github.com/F33D3R-Inc/facets/…"`). Four kinds, already in the
language:

- **component** — a presentational fragment, `component Avatar(name: text):` …,
  invoked with `use`. The bulk of the atoms.
- **ui facet** — `ui Nav in nav:` — a socketable presentation block.
- **data facet** — `data Feed in feed:` — a full vertical slice (entities +
  actions + policies + UI) that snaps into a wireframe socket.
- **wireframe / playground** — app skeletons (e.g. a Twitter-style 3-column shell).

Every facet is a valid, runnable `.fct` module on its own (so it can be built and
tested in isolation) and is **placement-sound across the import boundary** — the
compiler guarantees an imported server action can't read your `@client` state.

The library dogfoods everything shipped v1.10–v1.16: `icon` · `badge` · `tabs` ·
`richtext` · `video` · `image` · filtered `count`/`exists` · `match` ·
`pending`/`failed` · `in` · joins · dynamic `limit`/search.

---

## 2. Repo & structure

**A first-party monorepo: `github.com/F33D3R-Inc/facets`** (the standard library),
resolved by the existing registry (it already supports subpath imports). Versioned
as one unit — like a stdlib — independent of the language version.

```
facets/
  facet.json                 # manifest (name = repo path, version, facet >=1.16.0)
  README.md
  ui/        avatar.fct  badge.fct  card.fct  chip.fct  spinner.fct  …
  layout/    shell.fct   modal.fct  drawer.fct  grid.fct  …
  forms/     field.fct   searchbox.fct  toggle.fct  submit.fct  …
  social/    postcard.fct  engagementbar.fct  feed.fct  compose.fct  thread.fct  …
  profile/   header.fct  followbutton.fct  verifiedbadge.fct  …
  media/     gallery.fct  videoplayer.fct  lightbox.fct  …
  notify/    notification.fct  unreadbadge.fct  toast.fct  dmthread.fct  …
  data/      table.fct  list.fct  pagination.fct  emptystate.fct  …
  commerce/  subscribe.fct  tip.fct  wallet.fct  paywall.fct  …
  auth/      loginform.fct  signup.fct  sessionmenu.fct  …
  dash/      statcard.fct  leaderboard.fct  activity.fct  …
```

Import by subpath: `import "github.com/F33D3R-Inc/facets/social/postcard.fct"`.
Naming: PascalCase facet names, kebab/lowercase files & paths.

> The repo doesn't exist yet and `gh` isn't installed here, so step 0 is: create
> `F33D3R-Inc/facets` (you, or me via `gh` once available). Until then I prototype
> the first batch **locally** in this repo under `library/`, validate with
> `facet build`/`dev`, then move/publish to the facets repo.

---

## 3. Categories → 250+ (rough counts)

| Category | ~count | Examples |
|---|--:|---|
| UI atoms | 40 | avatar, badge×, button×, chip, tag, tooltip, card, pill, spinner, skeleton, divider, progress, rating, breadcrumb |
| Layout | 20 | shell (3-col), grid, stack, sidebar, header, footer, drawer, modal, sheet, masonry |
| Forms | 30 | text/textarea/password field, searchbox, select, checkbox, radio, toggle, date, upload, form-group, validation-msg, submit-pending, autosave |
| Social / feed | 40 | postcard, engagementbar, like/repost/bookmark, reply, thread, compose, feed (for-you/following), trends, who-to-follow, hashtag, mention, share |
| Identity / profile | 25 | profile-header, banner, bio, follow-button, follower-count, verified-badge, user-chip, settings |
| Media | 20 | image, gallery, video-player, audio, lightbox, carousel, embed |
| Notify / messaging | 20 | notification-item, unread-badge, toast, inbox, dm-thread, message-bubble, presence, typing |
| Data / CRUD | 25 | table, list, pagination, infinite-scroll, filter-bar, sort-header, results, empty-state, detail, editable-cell |
| Commerce | 15 | subscribe, tip, wallet, price, plan-card, checkout, paywall |
| Auth | 15 | login, signup, oauth, MFA, reset, session-menu |
| Dashboard | 15 | stat-card, chart, leaderboard, activity, metric |
| **Total** | **~265** | |

---

## 4. First batch — the f33d3r core (v1.17.0, ~18 facets)

Exactly what the screenshots need to rebuild f33d3r's home + profile end-to-end —
and a full exercise of the v1.16 language:

1. **Avatar** — rounded `image`, seed/url.
2. **VerifiedBadge** — `badge` / `icon`.
3. **IconNav** — left rail, `icon` + `link` + unread `badge`.
4. **PostCard** — author, time, body, media.
5. **EngagementBar** — reply/repost/like/bookmark counts (`count(x in … where …)`) + has-acted (`exists`).
6. **ComposeBox** — `input` + post button + `pending`/`failed`.
7. **TabbedFeed** — Following/Trending/New/NSFW via `tabs`.
8. **Feed** — for-you (all) vs following (`for … where exists(follow)`), `by … desc limit shown`.
9. **RichPost** — long-form via `richtext`.
10. **VideoPost** — `video`.
11. **PostKindSwitch** — `match post.kind: text/image/video/article`.
12. **Trends** — "What's happening".
13. **WhoToFollow** — list + FollowButton.
14. **NotificationItem + UnreadBadge** — fan-out rows + `count` badge.
15. **ProfileHeader** — banner, avatar, name, verified, follow/subscribe/tip.
16. **FollowButton** — follow/unfollow (join entity + `exists`).
17. **SearchBox** — `input` + `contains` filter.
18. **Shell** — the 3-column `wireframe`/`playground` they snap into.

Delivering this batch == proving the language can rebuild f33d3r, and seeds the
registry with the highest-traffic pieces.

---

## 5. Quality bar (per facet)

- Compiles (`facet build`); runs in isolation (`facet dev`) where it makes sense.
- A doc comment + a usage snippet in the category README.
- Data facets get a `facet test` behavior test.
- Placement-sound by construction (the compiler enforces it across imports).

---

## 6. Versioning & rollout

- The **facets repo versions independently** (start `v0.1.0`), language is the
  floor (`"facet": ">=1.16.0"` in the manifest). Each batch = a minor bump.
- **The "v1.17.0 milestone"** = the library's debut. Open question: do we also cut a
  *language* v1.17.0 (small — any registry ergonomics the library needs + a README
  pointer), or keep the language at v1.16 and let the library carry its own version?
  Recommend: **library carries its own version; language stays v1.16** unless the
  library surfaces a real language/registry gap (then a patch).
- Rollout in batches: **v0.1.0 = the f33d3r core (§4)**, then category by category
  (§3) toward 250+, each its own release.

---

## 7. Risks / watch-items

- **250 is a lot of content.** Guard against filler — every facet must earn its
  place by being used (f33d3r first, then real demand). Count is a destination, not
  a KPI.
- **Building the library will surface language gaps.** That's good — it's the same
  cycle: hit a wall → fix the lang → release → resume. Expect occasional language
  patch releases driven by the library.
- **Repo creation is blocked on `gh`/you.** Mitigation: prototype the core batch
  locally under `library/` first.

---

## 8. Immediate next steps

1. Decide: repo location (separate `F33D3R-Inc/facets` vs local `library/` to
   start) and versioning (library-independent — recommended).
2. Create/scaffold the repo (or `library/`) + `facet.json`.
3. Build the **f33d3r core batch (§4)**, each validated with `facet build`.
4. Publish `facets v0.1.0`; rebuild f33d3r's home screen from imports as the proof.
5. Expand category by category toward 250+.

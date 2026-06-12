# Standard Library

`github.com/F33D3R-Inc/fct/std` ships **229 compiled-and-tested facets** plus a
default responsive theme — enough surface to assemble a real social, video, or
live-streaming product without writing a component from scratch.

## Using it

A scaffolded project is already wired:

```go
c, err := std.CompileDir("facets")   // your facets, compiled ON TOP of the stdlib
…
app.MountRouter(mux, fa.ShellOptions{CSS: template.CSS(std.CSS) + appCSS})
```

That gives every one of your `.fct` files access to the whole catalog **by
name** — as child facets:

```
facet Profile:
    what:
        user: User
    looks:
        <Card title="{user.name}">
            <Avatar src="{user.avatar_url}" size="64"/>
            <Badge label="{user.role}"/>
        </Card>
```

…or rendered directly from Go: `c.Render("Button", map[string]any{…})`.
`std.Names()` lists everything; `std.Source()` returns the raw FDL if you
compile manually with `fa.Compile`.

`std.CSS` is the default theme — responsive out of the box (the `AppShell`
collapses 3-column → no right rail → icon nav → single column + bottom nav as
the viewport narrows). Add your own CSS after it to override.

## The catalog (by area)

- **atomic** — Button, IconButton, Icon, Avatar, Badge, Tag, Count, Spinner,
  Skeleton, Divider, Link, Toggle
- **feedback / state** — Alert, Toast, Banner, EmptyState, ProgressBar,
  ErrorState, RetryCard, LoadMoreButton, NewItemsPill, Paginator,
  SkeletonCard, OfflineBanner, EndOfFeed
- **layout / app shell** (slot-based) — Card, Stack, Row, Modal, AppShell,
  LeftRail, MainColumn, RightRail, RightRailCard, NavRail, NavRailItem,
  FeedTabs, Sidebar, TopBar, BottomNav, Grid, ScrollArea, BackBar,
  SectionHeader
- **form** — FormField, TextInput, TextArea, Checkbox, SubmitButton, Switch,
  RadioOption, Slider, FileUpload, SearchInput, OTPInput, CharCounter,
  FieldError
- **nav / search** — NavBar, NavItem, TabBar, Tab, Crumb, SearchBar,
  SearchResultPerson, TrendingItem, ExploreTile, CategoryChips
- **feed / compose** — PostCard, PostHeader, PostBody, PostActionBar,
  QuotedPost, CommentItem, WhoToFollowRow, Timeline, Composer, ReplyComposer,
  QuoteComposer, Poll, PollEditor, MediaPreview
- **media / video** — Image, VideoPlayer, AudioPlayer, GifPlayer, LinkPreview,
  SensitiveVeil, VideoControls, Scrubber, MediaGrid, Carousel, VideoCard,
  ChannelHeader, EngagementBar, ChapterList, ShortCard, PlaylistCard,
  WatchNextList
- **stories** — StoryRing, StoryBar, StoryProgress, StoryViewer
- **live / streaming** — LiveBadge, ViewerCount, LiveStreamPlayer, LiveChat,
  ChatMessage, TipButton, TipGoal, GiftTray, GoLiveButton, BroadcastControls,
  ReactionRail, PrivateShowBar, TokenBalance
- **commerce** — CoinBalance, SubscribeButton, Paywall, WalletCard, PriceTag,
  GiftItem
- **audio rooms** — SpaceCard, SpaceBar, SpeakerGrid, SpeakerTile, MicButton,
  RaiseHandButton, RoomControls
- **comments** — CommentThread, CommentNode, CommentComposer, CommentVote,
  PinnedComment
- **profile** — ProfileHeader, ProfileTabs, ProfileStats, BioBlock,
  ProfileSummary
- **overlays** — Sheet, Drawer, Dropdown, ContextMenu, Tooltip, ConfirmDialog,
  Lightbox, EmojiPicker, GifPicker
- **notifications** — NotificationItem, NotificationList, NotificationBell,
  ToastStack
- **settings** — SettingsSection, SettingsRow, SettingsToggleRow,
  AccountSwitchRow
- **analytics / studio** — StatCard, AnalyticsCard, KPIRow, Table, Sparkline,
  MetricDelta
- **status atoms** — StatusDot, Pill, Stars, Chip, VerifiedTick

Per-instance ids are wired where it matters — e.g. liking one `PostCard`
updates that one card surgically.

## Discovering props

Each stdlib facet's `what:` block is its documentation. Three ways to look:

```sh
fct audit <(echo …)          # access-control surface
```

- read the `.fct` source in the `std/` directory of the framework repo;
- `std.Names()` at runtime to enumerate;
- the VS Code extension (`editor/vscode`) gives diagnostics when you pass an
  unknown prop — composition is compile-checked, so a wrong prop name fails
  loudly at startup, not silently in production.

## Community packages

Beyond the stdlib, facets are shareable like npm packages:

```sh
fct search post              # discover
fct add social/post-card     # install into facets/ (validated on install)
fct init my-pkg ns/name      # author your own
fct pack my-pkg && fct publish my-pkg
```

The registry refuses packages that don't compile. `fct registry ./store`
self-hosts one.

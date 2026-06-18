'use strict';
// Demo app on the Node runtime. Build the IR first:
//
//   fct build ../../../cmd/fct/scaffold/facets/like_button.fct generated
//
// (with an fct.toml whose [compiler] target = "node", so render.json is emitted),
// then: node demo/server.js — open http://localhost:7373 and click the heart.

const { App } = require('../facet');

// In-memory state for the single demo post (a real app reads a DB).
const state = { post: { id: 'p1' }, count: 3, liked: false };

const app = new App({
  genDir: __dirname + '/generated',
  faKey: process.env.FA_SIGNING_KEY || '', // dev: unsigned unless a key is set
  title: 'LikeButton — Node runtime',
});

app.root('LikeButton', () => ({ post: state.post, count: state.count, liked: state.liked }));

// `when post.like:` — toggle the like and re-render. The runtime applies the
// facet's declared `replace LikeButton` mutation and pushes the signed fragment.
app.on('post.like', () => {
  state.liked = !state.liked;
  state.count += state.liked ? 1 : -1;
  return { post: state.post, count: state.count, liked: state.liked };
});

app.listen('localhost:7373');

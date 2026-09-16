// Code generated from an FCT wire schema. DO NOT EDIT.
// Source: SCHEMA_IDL_SCOPE.md Tier C (FCT-as-IDL).

export type Visibility = "Private" | "Public";

export type Role = "User" | "Admin";

export type ReferentialAction = "cascade" | "restrict" | "set_null";

export interface Coordinate {
  x: number;
  y: number;
  z: number;
  q: number;
}

export interface Node {
  address: string;
  coordinate: Coordinate;
  value: number;
  kind: string;
  data: string;
  owner: string;
  claimed_by?: string;
  visibility: Visibility;
}

export interface Edge {
  from: string;
  to: string;
  kind: string;
  owner: string;
}

export interface UserRecord {
  token_hash: string;
  owner: string;
  role: Role;
}

export interface HistoryEntry {
  address: string;
  archived_at_unix: number;
  node: Node;
  version: number;
}

export interface Expr {
  kind: string;
  val?: unknown;
  vtype?: string;
  name?: string;
  field?: string;
  obj?: Expr;
  key?: Expr;
  op?: string;
  args?: Expr[];
  l?: Expr;
  r?: Expr;
  x?: Expr;
  var?: string;
  where?: Expr;
}

export interface TransactionRequest {
  operations: TxOp[];
}

export interface EdgeSpec {
  to: string;
  kind: string;
}

export interface CreateNodeRequest {
  address: string;
  kind: string;
  x: number;
  y: number;
  z: number;
  q: number;
  data: string;
  public?: boolean;
  edges?: EdgeSpec[]; // default: []
  if_absent?: boolean; // default: false
}

export interface UpdateNodeRequest {
  data: string;
  public?: boolean;
}

export interface CreateEdgeRequest {
  from: string;
  to: string;
  kind: string;
}

export interface DeleteEdgeRequest {
  from: string;
  to: string;
  kind: string;
}

export interface CreateNodeResponse {
  address: string;
  edges_created: Edge[];
}

export interface CreateNodeError {
  error: string;
  edges_created_before_failure: Edge[];
}

// Bound from the URL query string, never a JSON body.
export interface QueryParams {
  kind?: string;
  owner?: string;
  limit?: number;
  offset?: number;
}

export interface QueryWhereRequest {
  kind?: string;
  owner?: string;
  where?: Expr;
  item_var?: string; // default: "item"
  order?: string;
  desc?: boolean; // default: false
  after?: string;
  limit?: number;
  offset?: number;
}

export interface QueryPage {
  nodes: Node[];
  next: string;
  examined: number;
}

export interface CountRequest {
  kind?: string;
  owner?: string;
  where?: Expr;
  item_var?: string; // default: "item"
}

export interface CountResponse {
  count: number;
}

export interface GroupCount {
  value?: unknown;
  count: number;
}

export interface CountByRequest {
  kind?: string;
  owner?: string;
  where?: Expr;
  item_var?: string; // default: "item"
  group_by: string;
  values?: unknown[];
}

export interface CountByResponse {
  counts: GroupCount[];
}

export interface AggregateRequest {
  kind?: string;
  owner?: string;
  where?: Expr;
  item_var?: string; // default: "item"
  func: string;
  field?: string;
}

export interface AggregateResponse {
  result: unknown;
}

export interface GroupAggregate {
  value?: unknown;
  result: unknown;
}

export interface AggregateByRequest {
  kind?: string;
  owner?: string;
  where?: Expr;
  item_var?: string; // default: "item"
  group_by: string;
  values?: unknown[];
  func: string;
  field?: string;
}

export interface AggregateByResponse {
  groups: GroupAggregate[];
}

export interface SequenceRequest {
  count?: number; // default: 1
}

export interface SequenceResponse {
  first: number;
  count: number;
}

export interface MultiGetRequest {
  addresses: string[];
}

export interface PublishRequest {
  payload: string;
}

// Bound from the URL query string, never a JSON body.
export interface EventsQuery {
  after?: number;
}

// Bound from the URL query string, never a JSON body.
export interface ChangesQuery {
  after?: number;
  limit?: number;
}

export interface CreateUserRequest {
  owner: string;
  role?: string;
}

export interface CreateUserResponse {
  owner: string;
  role: Role;
  token: string;
}

export interface UserSummary {
  owner: string;
  role: Role;
}

export interface CreateIndexRequest {
  name: string;
  kind: string;
  field: string;
  unique?: boolean; // default: false
  mode?: string;
}

export interface CreateReferenceRequest {
  name: string;
  kind: string;
  field: string;
  parent_kind: string;
  parent_field?: string;
  on_delete: ReferentialAction;
}

export type TxOp = TxOpInsertNode | TxOpInsertEdge | TxOpDeleteEdge | TxOpDeleteNode | TxOpClearKind | TxOpDeleteWhere | TxOpSetIf;

export interface TxOpInsertNode {
  type: "insert_node";
  address: string;
  kind: string;
  x: number;
  y: number;
  z: number;
  q: number;
  data: string;
  public?: boolean;
  owner?: string;
  claimed_by?: string;
}

export interface TxOpInsertEdge {
  type: "insert_edge";
  from: string;
  to: string;
  kind: string;
}

export interface TxOpDeleteEdge {
  type: "delete_edge";
  from: string;
  to: string;
  kind: string;
}

export interface TxOpDeleteNode {
  type: "delete_node";
  address: string;
}

export interface TxOpClearKind {
  type: "clear_kind";
  kind: string;
}

export interface TxOpDeleteWhere {
  type: "delete_where";
  kind: string;
  where?: Expr;
}

export interface TxOpSetIf {
  type: "set_if";
  address: string;
  field: string;
  expect_le?: number;
  expect_eq?: unknown;
  expect_absent?: boolean;
  set?: unknown; // default: {}
}


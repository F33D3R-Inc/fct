// Code generated from an FCT wire schema. DO NOT EDIT.
// Source: SCHEMA_IDL_SCOPE.md Tier C (FCT-as-IDL).

package wire

import (
	"encoding/json"
	"fmt"
)

type Visibility string

const (
	VisibilityPrivate Visibility = "Private"
	VisibilityPublic  Visibility = "Public"
)

type Role string

const (
	RoleUser  Role = "User"
	RoleAdmin Role = "Admin"
)

type ReferentialAction string

const (
	ReferentialActionCascade  ReferentialAction = "cascade"
	ReferentialActionRestrict ReferentialAction = "restrict"
	ReferentialActionSetNull  ReferentialAction = "set_null"
)

type Coordinate struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
	Z int64 `json:"z"`
	Q int64 `json:"q"`
}

type Node struct {
	Address    string      `json:"address"`
	Coordinate *Coordinate `json:"coordinate"`
	Value      int64       `json:"value"`
	Kind       string      `json:"kind"`
	Data       string      `json:"data"`
	Owner      string      `json:"owner"`
	ClaimedBy  string      `json:"claimed_by,omitempty"`
	Visibility Visibility  `json:"visibility"`
}

type Edge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Kind  string `json:"kind"`
	Owner string `json:"owner"`
}

type UserRecord struct {
	TokenHash string `json:"token_hash"`
	Owner     string `json:"owner"`
	Role      Role   `json:"role"`
}

type HistoryEntry struct {
	Address        string `json:"address"`
	ArchivedAtUnix int64  `json:"archived_at_unix"`
	Node           *Node  `json:"node"`
	Version        int64  `json:"version"`
}

type Expr struct {
	Kind  string          `json:"kind"`
	Val   json.RawMessage `json:"val,omitempty"`
	Vtype string          `json:"vtype,omitempty"`
	Name  string          `json:"name,omitempty"`
	Field string          `json:"field,omitempty"`
	Obj   *Expr           `json:"obj,omitempty"`
	Key   *Expr           `json:"key,omitempty"`
	Op    string          `json:"op,omitempty"`
	Args  []Expr          `json:"args,omitempty"`
	L     *Expr           `json:"l,omitempty"`
	R     *Expr           `json:"r,omitempty"`
	X     *Expr           `json:"x,omitempty"`
	Var   string          `json:"var,omitempty"`
	Where *Expr           `json:"where,omitempty"`
}

type TransactionRequest struct {
	Operations []TxOp `json:"operations"`
}

type EdgeSpec struct {
	To   string `json:"to"`
	Kind string `json:"kind"`
}

type CreateNodeRequest struct {
	Address  string     `json:"address"`
	Kind     string     `json:"kind"`
	X        int64      `json:"x"`
	Y        int64      `json:"y"`
	Z        int64      `json:"z"`
	Q        int64      `json:"q"`
	Data     string     `json:"data"`
	Public   bool       `json:"public,omitempty"`
	Edges    []EdgeSpec `json:"edges"`
	IfAbsent bool       `json:"if_absent"`
}

func (v *CreateNodeRequest) UnmarshalJSON(data []byte) error {
	type alias CreateNodeRequest
	aux := alias{
		Edges:    []EdgeSpec{},
		IfAbsent: false,
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*v = CreateNodeRequest(aux)
	return nil
}

type UpdateNodeRequest struct {
	Data   string `json:"data"`
	Public bool   `json:"public,omitempty"`
}

type CreateEdgeRequest struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}

type DeleteEdgeRequest struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}

type CreateNodeResponse struct {
	Address      string `json:"address"`
	EdgesCreated []Edge `json:"edges_created"`
}

type CreateNodeError struct {
	Error                     string `json:"error"`
	EdgesCreatedBeforeFailure []Edge `json:"edges_created_before_failure"`
}

// Bound from the URL query string, never a JSON body.
type QueryParams struct {
	Kind   string `json:"kind,omitempty"`
	Owner  string `json:"owner,omitempty"`
	Limit  int64  `json:"limit,omitempty"`
	Offset int64  `json:"offset,omitempty"`
}

type QueryWhereRequest struct {
	Kind    string `json:"kind,omitempty"`
	Owner   string `json:"owner,omitempty"`
	Where   *Expr  `json:"where,omitempty"`
	ItemVar string `json:"item_var"`
	Order   string `json:"order,omitempty"`
	Desc    bool   `json:"desc"`
	After   string `json:"after,omitempty"`
	Limit   int64  `json:"limit,omitempty"`
	Offset  int64  `json:"offset,omitempty"`
}

func (v *QueryWhereRequest) UnmarshalJSON(data []byte) error {
	type alias QueryWhereRequest
	aux := alias{
		ItemVar: "item",
		Desc:    false,
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*v = QueryWhereRequest(aux)
	return nil
}

type QueryPage struct {
	Nodes    []Node `json:"nodes"`
	Next     string `json:"next"`
	Examined int64  `json:"examined"`
}

type CountRequest struct {
	Kind    string `json:"kind,omitempty"`
	Owner   string `json:"owner,omitempty"`
	Where   *Expr  `json:"where,omitempty"`
	ItemVar string `json:"item_var"`
}

func (v *CountRequest) UnmarshalJSON(data []byte) error {
	type alias CountRequest
	aux := alias{
		ItemVar: "item",
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*v = CountRequest(aux)
	return nil
}

type CountResponse struct {
	Count int64 `json:"count"`
}

type GroupCount struct {
	Value json.RawMessage `json:"value,omitempty"`
	Count int64           `json:"count"`
}

type CountByRequest struct {
	Kind    string            `json:"kind,omitempty"`
	Owner   string            `json:"owner,omitempty"`
	Where   *Expr             `json:"where,omitempty"`
	ItemVar string            `json:"item_var"`
	GroupBy string            `json:"group_by"`
	Values  []json.RawMessage `json:"values,omitempty"`
}

func (v *CountByRequest) UnmarshalJSON(data []byte) error {
	type alias CountByRequest
	aux := alias{
		ItemVar: "item",
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*v = CountByRequest(aux)
	return nil
}

type CountByResponse struct {
	Counts []GroupCount `json:"counts"`
}

type AggregateRequest struct {
	Kind    string `json:"kind,omitempty"`
	Owner   string `json:"owner,omitempty"`
	Where   *Expr  `json:"where,omitempty"`
	ItemVar string `json:"item_var"`
	Func    string `json:"func"`
	Field   string `json:"field,omitempty"`
}

func (v *AggregateRequest) UnmarshalJSON(data []byte) error {
	type alias AggregateRequest
	aux := alias{
		ItemVar: "item",
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*v = AggregateRequest(aux)
	return nil
}

type AggregateResponse struct {
	Result json.RawMessage `json:"result"`
}

type GroupAggregate struct {
	Value  json.RawMessage `json:"value,omitempty"`
	Result json.RawMessage `json:"result"`
}

type AggregateByRequest struct {
	Kind    string            `json:"kind,omitempty"`
	Owner   string            `json:"owner,omitempty"`
	Where   *Expr             `json:"where,omitempty"`
	ItemVar string            `json:"item_var"`
	GroupBy string            `json:"group_by"`
	Values  []json.RawMessage `json:"values,omitempty"`
	Func    string            `json:"func"`
	Field   string            `json:"field,omitempty"`
}

func (v *AggregateByRequest) UnmarshalJSON(data []byte) error {
	type alias AggregateByRequest
	aux := alias{
		ItemVar: "item",
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*v = AggregateByRequest(aux)
	return nil
}

type AggregateByResponse struct {
	Groups []GroupAggregate `json:"groups"`
}

type SequenceRequest struct {
	Count int64 `json:"count"`
}

func (v *SequenceRequest) UnmarshalJSON(data []byte) error {
	type alias SequenceRequest
	aux := alias{
		Count: 1,
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*v = SequenceRequest(aux)
	return nil
}

type SequenceResponse struct {
	First int64 `json:"first"`
	Count int64 `json:"count"`
}

type MultiGetRequest struct {
	Addresses []string `json:"addresses"`
}

type PublishRequest struct {
	Payload string `json:"payload"`
}

// Bound from the URL query string, never a JSON body.
type EventsQuery struct {
	After int64 `json:"after,omitempty"`
}

// Bound from the URL query string, never a JSON body.
type ChangesQuery struct {
	After int64 `json:"after,omitempty"`
	Limit int64 `json:"limit,omitempty"`
}

type CreateUserRequest struct {
	Owner string `json:"owner"`
	Role  string `json:"role,omitempty"`
}

type CreateUserResponse struct {
	Owner string `json:"owner"`
	Role  Role   `json:"role"`
	Token string `json:"token"`
}

type UserSummary struct {
	Owner string `json:"owner"`
	Role  Role   `json:"role"`
}

type CreateIndexRequest struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Field  string `json:"field"`
	Unique bool   `json:"unique"`
	Mode   string `json:"mode,omitempty"`
}

func (v *CreateIndexRequest) UnmarshalJSON(data []byte) error {
	type alias CreateIndexRequest
	aux := alias{
		Unique: false,
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*v = CreateIndexRequest(aux)
	return nil
}

type CreateReferenceRequest struct {
	Name        string            `json:"name"`
	Kind        string            `json:"kind"`
	Field       string            `json:"field"`
	ParentKind  string            `json:"parent_kind"`
	ParentField string            `json:"parent_field,omitempty"`
	OnDelete    ReferentialAction `json:"on_delete"`
}

type TxOp struct {
	Type         string
	Address      string
	Kind         string
	X            int64
	Y            int64
	Z            int64
	Q            int64
	Data         string
	Public       bool
	Owner        string
	ClaimedBy    string
	From         string
	To           string
	Where        *Expr
	Field        string
	ExpectLe     float64
	ExpectEq     json.RawMessage
	ExpectAbsent bool
	Set          json.RawMessage
}

func (m TxOp) MarshalJSON() ([]byte, error) {
	switch m.Type {
	case "insert_node":
		return json.Marshal(struct {
			Type      string `json:"type"`
			Address   string `json:"address"`
			Kind      string `json:"kind"`
			X         int64  `json:"x"`
			Y         int64  `json:"y"`
			Z         int64  `json:"z"`
			Q         int64  `json:"q"`
			Data      string `json:"data"`
			Public    bool   `json:"public,omitempty"`
			Owner     string `json:"owner,omitempty"`
			ClaimedBy string `json:"claimed_by,omitempty"`
		}{
			Type:      m.Type,
			Address:   m.Address,
			Kind:      m.Kind,
			X:         m.X,
			Y:         m.Y,
			Z:         m.Z,
			Q:         m.Q,
			Data:      m.Data,
			Public:    m.Public,
			Owner:     m.Owner,
			ClaimedBy: m.ClaimedBy,
		})
	case "insert_edge":
		return json.Marshal(struct {
			Type string `json:"type"`
			From string `json:"from"`
			To   string `json:"to"`
			Kind string `json:"kind"`
		}{
			Type: m.Type,
			From: m.From,
			To:   m.To,
			Kind: m.Kind,
		})
	case "delete_edge":
		return json.Marshal(struct {
			Type string `json:"type"`
			From string `json:"from"`
			To   string `json:"to"`
			Kind string `json:"kind"`
		}{
			Type: m.Type,
			From: m.From,
			To:   m.To,
			Kind: m.Kind,
		})
	case "delete_node":
		return json.Marshal(struct {
			Type    string `json:"type"`
			Address string `json:"address"`
		}{
			Type:    m.Type,
			Address: m.Address,
		})
	case "clear_kind":
		return json.Marshal(struct {
			Type string `json:"type"`
			Kind string `json:"kind"`
		}{
			Type: m.Type,
			Kind: m.Kind,
		})
	case "delete_where":
		return json.Marshal(struct {
			Type  string `json:"type"`
			Kind  string `json:"kind"`
			Where *Expr  `json:"where,omitempty"`
		}{
			Type:  m.Type,
			Kind:  m.Kind,
			Where: m.Where,
		})
	case "set_if":
		return json.Marshal(struct {
			Type         string          `json:"type"`
			Address      string          `json:"address"`
			Field        string          `json:"field"`
			ExpectLe     float64         `json:"expect_le,omitempty"`
			ExpectEq     json.RawMessage `json:"expect_eq,omitempty"`
			ExpectAbsent bool            `json:"expect_absent,omitempty"`
			Set          json.RawMessage `json:"set"`
		}{
			Type:         m.Type,
			Address:      m.Address,
			Field:        m.Field,
			ExpectLe:     m.ExpectLe,
			ExpectEq:     m.ExpectEq,
			ExpectAbsent: m.ExpectAbsent,
			Set:          m.Set,
		})
	default:
		return nil, fmt.Errorf("unknown TxOp variant %q", m.Type)
	}
}

func (m *TxOp) UnmarshalJSON(data []byte) error {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	switch probe.Type {
	case "insert_node":
		var v struct {
			Address   string `json:"address"`
			Kind      string `json:"kind"`
			X         int64  `json:"x"`
			Y         int64  `json:"y"`
			Z         int64  `json:"z"`
			Q         int64  `json:"q"`
			Data      string `json:"data"`
			Public    bool   `json:"public,omitempty"`
			Owner     string `json:"owner,omitempty"`
			ClaimedBy string `json:"claimed_by,omitempty"`
		}
		if err := json.Unmarshal(data, &v); err != nil {
			return err
		}
		*m = TxOp{Type: probe.Type, Address: v.Address, Kind: v.Kind, X: v.X, Y: v.Y, Z: v.Z, Q: v.Q, Data: v.Data, Public: v.Public, Owner: v.Owner, ClaimedBy: v.ClaimedBy}
		return nil
	case "insert_edge":
		var v struct {
			From string `json:"from"`
			To   string `json:"to"`
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(data, &v); err != nil {
			return err
		}
		*m = TxOp{Type: probe.Type, From: v.From, To: v.To, Kind: v.Kind}
		return nil
	case "delete_edge":
		var v struct {
			From string `json:"from"`
			To   string `json:"to"`
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(data, &v); err != nil {
			return err
		}
		*m = TxOp{Type: probe.Type, From: v.From, To: v.To, Kind: v.Kind}
		return nil
	case "delete_node":
		var v struct {
			Address string `json:"address"`
		}
		if err := json.Unmarshal(data, &v); err != nil {
			return err
		}
		*m = TxOp{Type: probe.Type, Address: v.Address}
		return nil
	case "clear_kind":
		var v struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(data, &v); err != nil {
			return err
		}
		*m = TxOp{Type: probe.Type, Kind: v.Kind}
		return nil
	case "delete_where":
		var v struct {
			Kind  string `json:"kind"`
			Where *Expr  `json:"where,omitempty"`
		}
		if err := json.Unmarshal(data, &v); err != nil {
			return err
		}
		*m = TxOp{Type: probe.Type, Kind: v.Kind, Where: v.Where}
		return nil
	case "set_if":
		var v struct {
			Address      string          `json:"address"`
			Field        string          `json:"field"`
			ExpectLe     float64         `json:"expect_le,omitempty"`
			ExpectEq     json.RawMessage `json:"expect_eq,omitempty"`
			ExpectAbsent bool            `json:"expect_absent,omitempty"`
			Set          json.RawMessage `json:"set"`
		}
		if err := json.Unmarshal(data, &v); err != nil {
			return err
		}
		*m = TxOp{Type: probe.Type, Address: v.Address, Field: v.Field, ExpectLe: v.ExpectLe, ExpectEq: v.ExpectEq, ExpectAbsent: v.ExpectAbsent, Set: v.Set}
		return nil
	default:
		return fmt.Errorf("unknown TxOp variant %q", probe.Type)
	}
}

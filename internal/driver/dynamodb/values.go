package dynamodb

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"rowsmith/internal/driver"
)

// Attribute types as DynamoDB names them.
const (
	tS, tN, tB, tBOOL, tNULL, tM, tL, tSS, tNS, tBS = "S", "N", "B", "BOOL", "NULL", "M", "L", "SS", "NS", "BS"
	tMixed                                          = "mixed"
)

func typeOf(v types.AttributeValue) string {
	switch v.(type) {
	case *types.AttributeValueMemberS:
		return tS
	case *types.AttributeValueMemberN:
		return tN
	case *types.AttributeValueMemberB:
		return tB
	case *types.AttributeValueMemberBOOL:
		return tBOOL
	case *types.AttributeValueMemberNULL:
		return tNULL
	case *types.AttributeValueMemberM:
		return tM
	case *types.AttributeValueMemberL:
		return tL
	case *types.AttributeValueMemberSS:
		return tSS
	case *types.AttributeValueMemberNS:
		return tNS
	case *types.AttributeValueMemberBS:
		return tBS
	}
	return tMixed
}

func kindOf(t string) driver.ValueKind {
	switch t {
	case tS:
		return driver.KindString
	case tN:
		return driver.KindDecimal // exact: numbers are decimal text
	case tB:
		return driver.KindBinary
	case tBOOL:
		return driver.KindBool
	case tM:
		return driver.KindObject
	case tL, tSS, tNS, tBS:
		return driver.KindArray
	}
	return driver.KindOther
}

// number writes a DynamoDB number as JSON can carry it: an integer or a
// float when exact, otherwise the text.
func number(n string) any {
	if i, err := strconv.ParseInt(n, 10, 64); err == nil {
		return driver.EncodeInt(i)
	}
	if f, err := strconv.ParseFloat(n, 64); err == nil && strconv.FormatFloat(f, 'f', -1, 64) == strings.TrimRight(strings.TrimRight(n, "0"), ".") {
		return f
	}
	return n
}

// cell encodes a top-level attribute for the grid; numbers stay exact text.
func cell(v types.AttributeValue) any {
	if n, ok := v.(*types.AttributeValueMemberN); ok {
		return n.Value
	}
	return nested(v)
}

// nested encodes a value inside a map or list, or a set member.
func nested(v types.AttributeValue) any {
	switch x := v.(type) {
	case nil:
		return nil
	case *types.AttributeValueMemberS:
		return driver.EncodeText(x.Value)
	case *types.AttributeValueMemberN:
		return number(x.Value)
	case *types.AttributeValueMemberB:
		return driver.EncodeBinary(x.Value)
	case *types.AttributeValueMemberBOOL:
		return x.Value
	case *types.AttributeValueMemberNULL:
		return nil
	case *types.AttributeValueMemberM:
		d := driver.Doc{Values: map[string]any{}}
		for k, e := range x.Value {
			d.Keys = append(d.Keys, k)
			d.Values[k] = nested(e)
		}
		sort.Strings(d.Keys)
		return d
	case *types.AttributeValueMemberL:
		out := make([]any, len(x.Value))
		for i, e := range x.Value {
			out[i] = nested(e)
		}
		return out
	case *types.AttributeValueMemberSS:
		out := make([]any, len(x.Value))
		for i, e := range x.Value {
			out[i] = e
		}
		return out
	case *types.AttributeValueMemberNS:
		out := make([]any, len(x.Value))
		for i, e := range x.Value {
			out[i] = number(e)
		}
		return out
	case *types.AttributeValueMemberBS:
		out := make([]any, len(x.Value))
		for i, e := range x.Value {
			out[i] = driver.EncodeBinary(e)
		}
		return out
	}
	return nil
}

// ---- values written back ---------------------------------------------------------

func numberText(v any) (string, error) {
	switch x := v.(type) {
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e15 {
			return strconv.FormatInt(int64(x), 10), nil
		}
		return strconv.FormatFloat(x, 'g', -1, 64), nil
	case json.Number:
		return x.String(), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case int:
		return strconv.Itoa(x), nil
	case string:
		s := strings.TrimSpace(x)
		if _, ok := new(big.Float).SetString(s); !ok || s == "" {
			return "", fmt.Errorf("%q is not a number", x)
		}
		return s, nil
	case bool:
		return "", errors.New("a boolean is not a number")
	}
	return "", fmt.Errorf("%v is not a number", v)
}

func binaryValue(v any) ([]byte, error) {
	switch x := v.(type) {
	case map[string]any:
		if b64, ok := x["$bin"].(string); ok {
			return base64.StdEncoding.DecodeString(b64)
		}
	case string:
		return []byte(x), nil
	}
	return nil, errors.New("not a binary value")
}

// parsed reads JSON text typed into a cell for a map, list or set.
func parsed(v any) any {
	if s, ok := v.(string); ok {
		t := strings.TrimSpace(s)
		if (strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")) && json.Valid([]byte(t)) {
			dec := json.NewDecoder(strings.NewReader(t))
			dec.UseNumber()
			var out any
			dec.Decode(&out)
			return out
		}
	}
	return v
}

// attribute turns an edited cell into an attribute of the column's type;
// for an attribute of no known type (or mixed), the value's own shape
// decides.
func attribute(typ string, v any) (types.AttributeValue, error) {
	if m, ok := v.(map[string]any); ok {
		switch {
		case m["$null"] == true:
			return &types.AttributeValueMemberNULL{Value: true}, nil
		case m["$expr"] != nil || m["$default"] == true:
			return nil, errors.New("DynamoDB stores values, not expressions")
		}
	}
	if v == nil {
		return &types.AttributeValueMemberNULL{Value: true}, nil
	}
	switch typ {
	case tS:
		switch x := v.(type) {
		case string:
			return &types.AttributeValueMemberS{Value: x}, nil
		case float64, json.Number, bool:
			return &types.AttributeValueMemberS{Value: fmt.Sprint(x)}, nil
		}
	case tN:
		n, err := numberText(v)
		if err != nil {
			return nil, err
		}
		return &types.AttributeValueMemberN{Value: n}, nil
	case tB:
		b, err := binaryValue(v)
		if err != nil {
			return nil, err
		}
		return &types.AttributeValueMemberB{Value: b}, nil
	case tBOOL:
		switch x := v.(type) {
		case bool:
			return &types.AttributeValueMemberBOOL{Value: x}, nil
		case string:
			b, err := strconv.ParseBool(strings.TrimSpace(x))
			if err != nil {
				return nil, fmt.Errorf("%q is not true or false", x)
			}
			return &types.AttributeValueMemberBOOL{Value: b}, nil
		}
	case tSS, tNS, tBS:
		arr, ok := parsed(v).([]any)
		if !ok {
			return nil, fmt.Errorf("a %s is a list of values, such as [\"a\", \"b\"]", map[string]string{tSS: "string set", tNS: "number set", tBS: "binary set"}[typ])
		}
		if len(arr) == 0 {
			return nil, errors.New("DynamoDB sets can't be empty; remove the attribute instead")
		}
		switch typ {
		case tSS:
			out := &types.AttributeValueMemberSS{}
			for _, e := range arr {
				out.Value = append(out.Value, fmt.Sprint(e))
			}
			return out, nil
		case tNS:
			out := &types.AttributeValueMemberNS{}
			for _, e := range arr {
				n, err := numberText(e)
				if err != nil {
					return nil, err
				}
				out.Value = append(out.Value, n)
			}
			return out, nil
		default:
			out := &types.AttributeValueMemberBS{}
			for _, e := range arr {
				b, err := binaryValue(e)
				if err != nil {
					return nil, err
				}
				out.Value = append(out.Value, b)
			}
			return out, nil
		}
	case tM, tL:
		v = parsed(v)
	}
	return infer(v)
}

// infer chooses an attribute type from a value's shape.
func infer(v any) (types.AttributeValue, error) {
	switch x := v.(type) {
	case nil:
		return &types.AttributeValueMemberNULL{Value: true}, nil
	case string:
		return &types.AttributeValueMemberS{Value: x}, nil
	case bool:
		return &types.AttributeValueMemberBOOL{Value: x}, nil
	case float64, json.Number, int64, int:
		n, err := numberText(fmt.Sprint(x))
		if err != nil {
			return nil, err
		}
		return &types.AttributeValueMemberN{Value: n}, nil
	case driver.Doc:
		m := map[string]any{}
		for _, k := range x.Keys {
			m[k] = x.Values[k]
		}
		return infer(m)
	case map[string]any:
		if b64, ok := x["$bin"].(string); ok && len(x) <= 3 {
			b, err := base64.StdEncoding.DecodeString(b64)
			if err != nil {
				return nil, err
			}
			return &types.AttributeValueMemberB{Value: b}, nil
		}
		if gj, ok := x["$geo"]; ok { // a shape, as GeoJSON
			return infer(gj)
		}
		out := &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{}}
		for k, e := range x {
			a, err := infer(e)
			if err != nil {
				return nil, err
			}
			out.Value[k] = a
		}
		return out, nil
	case json.RawMessage:
		dec := json.NewDecoder(strings.NewReader(string(x)))
		dec.UseNumber()
		var parsedValue any
		if err := dec.Decode(&parsedValue); err != nil {
			return nil, err
		}
		return infer(parsedValue)
	case []any:
		out := &types.AttributeValueMemberL{Value: make([]types.AttributeValue, len(x))}
		for i, e := range x {
			a, err := infer(e)
			if err != nil {
				return nil, err
			}
			out.Value[i] = a
		}
		return out, nil
	}
	return &types.AttributeValueMemberS{Value: fmt.Sprint(v)}, nil
}

// ---- DynamoDB JSON ({"S": "x"}, {"N": "1"}, {"M": {…}}) --------------------------------

func toJSON(v types.AttributeValue) any {
	switch x := v.(type) {
	case *types.AttributeValueMemberS:
		return map[string]any{"S": x.Value}
	case *types.AttributeValueMemberN:
		return map[string]any{"N": x.Value}
	case *types.AttributeValueMemberB:
		return map[string]any{"B": base64.StdEncoding.EncodeToString(x.Value)}
	case *types.AttributeValueMemberBOOL:
		return map[string]any{"BOOL": x.Value}
	case *types.AttributeValueMemberNULL:
		return map[string]any{"NULL": true}
	case *types.AttributeValueMemberM:
		return map[string]any{"M": itemJSON(x.Value)}
	case *types.AttributeValueMemberL:
		out := make([]any, len(x.Value))
		for i, e := range x.Value {
			out[i] = toJSON(e)
		}
		return map[string]any{"L": out}
	case *types.AttributeValueMemberSS:
		return map[string]any{"SS": x.Value}
	case *types.AttributeValueMemberNS:
		return map[string]any{"NS": x.Value}
	case *types.AttributeValueMemberBS:
		out := make([]string, len(x.Value))
		for i, e := range x.Value {
			out[i] = base64.StdEncoding.EncodeToString(e)
		}
		return map[string]any{"BS": out}
	}
	return nil
}

func itemJSON(item map[string]types.AttributeValue) map[string]any {
	out := make(map[string]any, len(item))
	for k, v := range item {
		out[k] = toJSON(v)
	}
	return out
}

func fromJSON(v any) (types.AttributeValue, error) {
	m, ok := v.(map[string]any)
	if !ok || len(m) != 1 {
		return nil, fmt.Errorf("an attribute value is one typed member, such as {\"S\": \"text\"} or {\"N\": \"42\"}; got %v", v)
	}
	for t, x := range m {
		switch t {
		case tS:
			s, ok := x.(string)
			if !ok {
				return nil, errors.New(`"S" takes a string`)
			}
			return &types.AttributeValueMemberS{Value: s}, nil
		case tN:
			n, err := numberText(x)
			if err != nil {
				return nil, err
			}
			return &types.AttributeValueMemberN{Value: n}, nil
		case tB:
			s, _ := x.(string)
			b, err := base64.StdEncoding.DecodeString(s)
			if err != nil {
				return nil, errors.New(`"B" takes base64 text`)
			}
			return &types.AttributeValueMemberB{Value: b}, nil
		case tBOOL:
			b, ok := x.(bool)
			if !ok {
				return nil, errors.New(`"BOOL" takes true or false`)
			}
			return &types.AttributeValueMemberBOOL{Value: b}, nil
		case tNULL:
			return &types.AttributeValueMemberNULL{Value: true}, nil
		case tM:
			inner, ok := x.(map[string]any)
			if !ok {
				return nil, errors.New(`"M" takes an object of attribute values`)
			}
			item, err := itemFromJSON(inner)
			if err != nil {
				return nil, err
			}
			return &types.AttributeValueMemberM{Value: item}, nil
		case tL:
			arr, ok := x.([]any)
			if !ok {
				return nil, errors.New(`"L" takes a list of attribute values`)
			}
			out := &types.AttributeValueMemberL{}
			for _, e := range arr {
				a, err := fromJSON(e)
				if err != nil {
					return nil, err
				}
				out.Value = append(out.Value, a)
			}
			return out, nil
		case tSS, tNS, tBS:
			arr, ok := x.([]any)
			if !ok {
				return nil, fmt.Errorf("%q takes a list", t)
			}
			return attribute(t, arr)
		}
		return nil, fmt.Errorf("unknown attribute type %q", t)
	}
	return nil, nil
}

func itemFromJSON(m map[string]any) (map[string]types.AttributeValue, error) {
	out := make(map[string]types.AttributeValue, len(m))
	for k, v := range m {
		a, err := fromJSON(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		out[k] = a
	}
	return out, nil
}

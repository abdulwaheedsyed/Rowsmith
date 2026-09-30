package bigquery

import (
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/civil"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/geo"
)

// BigQuery GEOGRAPHY values are always WGS 84.
const geographySRID = 4326

// kindOf maps a field to the value kind the grid renders. RECORD columns are
// a single object cell and REPEATED fields a single array cell.
func kindOf(fs *bigquery.FieldSchema) driver.ValueKind {
	if fs.Repeated {
		return driver.KindArray
	}
	return scalarKind(fs.Type)
}

func scalarKind(t bigquery.FieldType) driver.ValueKind {
	switch t {
	case bigquery.StringFieldType, bigquery.RangeFieldType:
		return driver.KindString
	case bigquery.BytesFieldType:
		return driver.KindBinary
	case bigquery.IntegerFieldType:
		return driver.KindInt
	case bigquery.FloatFieldType:
		return driver.KindFloat
	case bigquery.BooleanFieldType:
		return driver.KindBool
	case bigquery.NumericFieldType, bigquery.BigNumericFieldType:
		return driver.KindDecimal
	case bigquery.DateFieldType:
		return driver.KindDate
	case bigquery.TimeFieldType:
		return driver.KindTime
	case bigquery.DateTimeFieldType:
		return driver.KindDateTime
	case bigquery.TimestampFieldType:
		return driver.KindTimestamp
	case bigquery.GeographyFieldType:
		return driver.KindGeometry
	case bigquery.JSONFieldType:
		return driver.KindJSON
	case bigquery.IntervalFieldType:
		return driver.KindInterval
	case bigquery.RecordFieldType:
		return driver.KindObject
	}
	return driver.KindOther
}

// standardNames translates the legacy names the API reports into GoogleSQL.
var standardNames = map[bigquery.FieldType]string{
	bigquery.IntegerFieldType: "INT64",
	bigquery.FloatFieldType:   "FLOAT64",
	bigquery.BooleanFieldType: "BOOL",
}

var plainIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// typeName renders a field's GoogleSQL type, e.g. ARRAY<STRUCT<a INT64, b STRING>>.
func typeName(fs *bigquery.FieldSchema) string {
	t := scalarTypeName(fs)
	if fs.Repeated {
		t = "ARRAY<" + t + ">"
	}
	return t
}

func scalarTypeName(fs *bigquery.FieldSchema) string {
	switch fs.Type {
	case bigquery.RecordFieldType:
		parts := make([]string, len(fs.Schema))
		for i, f := range fs.Schema {
			name := f.Name
			if !plainIdent.MatchString(name) {
				name = quote(name)
			}
			parts[i] = name + " " + typeName(f)
		}
		return "STRUCT<" + strings.Join(parts, ", ") + ">"
	case bigquery.RangeFieldType:
		if fs.RangeElementType != nil {
			return "RANGE<" + string(fs.RangeElementType.Type) + ">"
		}
	case bigquery.NumericFieldType, bigquery.BigNumericFieldType:
		switch {
		case fs.Precision > 0 && fs.Scale > 0:
			return fmt.Sprintf("%s(%d, %d)", fs.Type, fs.Precision, fs.Scale)
		case fs.Precision > 0:
			return fmt.Sprintf("%s(%d)", fs.Type, fs.Precision)
		}
	case bigquery.StringFieldType, bigquery.BytesFieldType:
		if fs.MaxLength > 0 {
			return fmt.Sprintf("%s(%d)", fs.Type, fs.MaxLength)
		}
	}
	if n, ok := standardNames[fs.Type]; ok {
		return n
	}
	return string(fs.Type)
}

// baseType is the lowercased type family, e.g. "int64", "struct", "array".
func baseType(fs *bigquery.FieldSchema) string {
	switch {
	case fs.Repeated:
		return "array"
	case fs.Type == bigquery.RecordFieldType:
		return "struct"
	}
	if n, ok := standardNames[fs.Type]; ok {
		return strings.ToLower(n)
	}
	return strings.ToLower(string(fs.Type))
}

// resultColumns describes the top-level fields of a result schema.
func resultColumns(s bigquery.Schema) []driver.ResultColumn {
	cols := make([]driver.ResultColumn, len(s))
	for i, fs := range s {
		nullable := !fs.Required && !fs.Repeated
		cols[i] = driver.ResultColumn{Name: fs.Name, Type: typeName(fs), Kind: kindOf(fs), Nullable: &nullable}
	}
	return cols
}

// encodeRow converts one row of client values into JSON-ready cells.
func encodeRow(row []bigquery.Value, s bigquery.Schema) []any {
	out := make([]any, len(row))
	for i, v := range row {
		if i < len(s) {
			out[i] = encodeValue(v, s[i])
		} else {
			out[i] = driver.EncodeText(fmt.Sprint(v))
		}
	}
	return out
}

// encodeValue converts a value returned by the client library according to
// its field schema: STRUCT becomes an object keyed by field name and ARRAY a
// JSON array, both encoded recursively.
func encodeValue(v bigquery.Value, fs *bigquery.FieldSchema) any {
	if v == nil {
		return nil
	}
	if fs.Repeated {
		items, ok := v.([]bigquery.Value)
		if !ok {
			return driver.EncodeText(fmt.Sprint(v))
		}
		elem := *fs
		elem.Repeated = false
		out := make([]any, len(items))
		for i, it := range items {
			out[i] = encodeValue(it, &elem)
		}
		return out
	}
	if fs.Type == bigquery.RecordFieldType {
		fields, ok := v.([]bigquery.Value)
		if !ok {
			return driver.EncodeText(fmt.Sprint(v))
		}
		out := make(map[string]any, len(fields))
		for i, f := range fields {
			if i < len(fs.Schema) {
				out[fs.Schema[i].Name] = encodeValue(f, fs.Schema[i])
			}
		}
		return out
	}
	return encodeScalar(v, fs.Type)
}

func encodeScalar(v bigquery.Value, t bigquery.FieldType) any {
	switch x := v.(type) {
	case int64:
		return driver.EncodeInt(x)
	case float64:
		return driver.EncodeFloat(x)
	case bool:
		return x
	case []byte:
		return driver.EncodeBinary(x)
	case *big.Rat:
		scale := bigquery.NumericScaleDigits
		if t == bigquery.BigNumericFieldType {
			scale = bigquery.BigNumericScaleDigits
		}
		return decimalString(x, scale)
	case civil.Date:
		return x.String()
	case civil.Time:
		return driver.EncodeTime(time.Date(0, 1, 1, x.Hour, x.Minute, x.Second, x.Nanosecond, time.UTC), driver.KindTime)
	case civil.DateTime:
		return driver.EncodeTime(x.In(time.UTC), driver.KindDateTime)
	case time.Time:
		return driver.EncodeTime(x.UTC(), driver.KindTimestamp)
	case *bigquery.IntervalValue:
		return x.String()
	case *bigquery.RangeValue:
		return rangeString(x, t)
	case string:
		if t == bigquery.GeographyFieldType {
			if cell, ok := geo.FromWKT("SRID=" + strconv.Itoa(geographySRID) + ";" + x); ok {
				return cell
			}
		}
		return driver.EncodeText(x)
	}
	return driver.EncodeText(fmt.Sprint(v))
}

// decimalString renders a NUMERIC/BIGNUMERIC exactly, without trailing zeros.
// Values read from BigQuery never have more fractional digits than scale.
func decimalString(r *big.Rat, scale int) string {
	if r.IsInt() {
		return r.Num().String()
	}
	s := r.FloatString(scale)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// rangeString renders a RANGE the way GoogleSQL prints it: [start, end).
func rangeString(r *bigquery.RangeValue, t bigquery.FieldType) string {
	bound := func(v bigquery.Value) string {
		if v == nil {
			return "UNBOUNDED"
		}
		return fmt.Sprint(encodeScalar(v, t))
	}
	return "[" + bound(r.Start) + ", " + bound(r.End) + ")"
}

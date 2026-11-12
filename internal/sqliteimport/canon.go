package sqliteimport

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash"
	"math"
	"strings"
)

// canon converts a driver value (modernc.org/sqlite or pgx) to the canonical
// Go value for a column kind: nil, int64, float64, string or []byte. The
// conversion must be exact; a value that would be rounded or reinterpreted is
// an error, so the import fails rather than carrying a changed value.
func canon(k kind, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch k {
	case kInt:
		switch x := v.(type) {
		case int64:
			return x, nil
		case int32:
			return int64(x), nil
		case int16:
			return int64(x), nil
		case int:
			return int64(x), nil
		case float64:
			if x == math.Trunc(x) && math.Abs(x) < 1<<53 {
				return int64(x), nil
			}
		}
	case kFloat:
		switch x := v.(type) {
		case float64:
			return x, nil
		case float32:
			return float64(x), nil
		case int64:
			if x > -(1<<53) && x < 1<<53 {
				return float64(x), nil
			}
		case int32:
			return float64(x), nil
		}
	case kText:
		switch x := v.(type) {
		case string:
			return x, nil
		case []byte:
			return string(x), nil
		}
	case kBytes:
		switch x := v.(type) {
		case []byte:
			return x, nil
		case string:
			return []byte(x), nil
		}
	}
	return nil, fmt.Errorf("value %v (%T) is not an exact %s", v, v, k)
}

func (k kind) String() string {
	switch k {
	case kInt:
		return "integer"
	case kFloat:
		return "double precision"
	case kText:
		return "text"
	case kBytes:
		return "bytea"
	}
	return "unknown"
}

// canonRow converts a whole row in place.
func canonRow(kinds []kind, row []any) error {
	if len(row) != len(kinds) {
		return fmt.Errorf("row has %d columns, want %d", len(row), len(kinds))
	}
	for n := range row {
		v, err := canon(kinds[n], row[n])
		if err != nil {
			return fmt.Errorf("column %d: %w", n+1, err)
		}
		row[n] = v
	}
	return nil
}

// hashValue writes one canonical value to h in an unambiguous encoding: a
// type tag, then a fixed-width number or a length-prefixed byte string.
func hashValue(h hash.Hash, v any) {
	var buf [binary.MaxVarintLen64 + 1]byte
	switch x := v.(type) {
	case nil:
		h.Write([]byte{0})
	case int64:
		buf[0] = 1
		binary.BigEndian.PutUint64(buf[1:9], uint64(x))
		h.Write(buf[:9])
	case float64:
		buf[0] = 2
		binary.BigEndian.PutUint64(buf[1:9], math.Float64bits(x))
		h.Write(buf[:9])
	case string:
		buf[0] = 3
		n := binary.PutUvarint(buf[1:], uint64(len(x)))
		h.Write(buf[:1+n])
		h.Write([]byte(x))
	case []byte:
		buf[0] = 4
		n := binary.PutUvarint(buf[1:], uint64(len(x)))
		h.Write(buf[:1+n])
		h.Write(x)
	default:
		panic(fmt.Sprintf("sqliteimport: non-canonical value %T", v))
	}
}

// equalCanon compares two canonical values exactly (floats bit for bit).
func equalCanon(a, b any) bool {
	switch x := a.(type) {
	case nil:
		return b == nil
	case int64:
		y, ok := b.(int64)
		return ok && x == y
	case float64:
		y, ok := b.(float64)
		return ok && math.Float64bits(x) == math.Float64bits(y)
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []byte:
		y, ok := b.([]byte)
		return ok && bytes.Equal(x, y)
	}
	return false
}

// describe renders a canonical value for a report, truncating blobs.
func describe(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return fmt.Sprintf("<%d bytes>", len(x))
	case string:
		if len(x) > 80 {
			return fmt.Sprintf("%q…", x[:80])
		}
		return fmt.Sprintf("%q", x)
	}
	return fmt.Sprint(v)
}

func describeKey(row []any, keyN int) string {
	if row == nil {
		return "-"
	}
	parts := make([]string, keyN)
	for n := 0; n < keyN; n++ {
		parts[n] = describe(row[n])
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

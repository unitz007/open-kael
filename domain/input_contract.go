package domain

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// The input half of the tool contract.
//
// A tool's InputSchema is what the model is told. CheckInput makes sure what the
// model sends back is what the schema says, before anything else happens: before
// the person is asked to approve a call, before the executor sees it. Every tool
// goes through it (see HydrateTool), whatever its source, so an executor can rely
// on the inputs it declared being present and of the declared type.
//
// It is forgiving about harmless differences models make, and exact about
// everything else:
//
//   - numbers may arrive as text ("5") or, for integers, as whole decimals (5.0);
//     booleans as "true" / "false"; text as a number or boolean. These are converted.
//   - a required input that is missing, null or empty text is an error.
//   - an input of the wrong kind, a value outside an enum, or a number with a
//     fraction where a whole number is required is an error.
//   - optional inputs that are null are dropped.
//   - inputs the schema doesn't declare are passed through untouched; the model was
//     told there are none, and an executor that reads one still gets it.
//
// Numbers stay float64 (what decoded JSON gives), so executors written against
// the decoded form keep working. Errors are written for the model: they say which
// input and what was wrong, so it can fix the call.

// CheckInput validates input against the schema (which must describe an object)
// and returns the normalised input. input is not modified.
func (s Schema) CheckInput(input map[string]any) (map[string]any, error) {
	var problems []string
	out := s.checkObject("", input, &problems)
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("invalid input: %s", strings.Join(problems, "; "))
	}
	return out, nil
}

func (s Schema) checkObject(path string, in map[string]any, problems *[]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v // undeclared inputs pass through
	}
	for name, prop := range s.Properties {
		v, present := in[name]
		if !present || v == nil {
			delete(out, name)
			continue
		}
		cv, err := prop.check(v)
		if err != nil {
			*problems = append(*problems, fmt.Sprintf("%s %s", quote(path+name), err.Error()))
			continue
		}
		out[name] = cv
	}
	for _, r := range s.Required {
		v, ok := out[r]
		if !ok || v == nil || v == "" {
			if !hasProblemFor(*problems, quote(path+r)) {
				*problems = append(*problems, fmt.Sprintf("%s is required", quote(path+r)))
			}
		}
	}
	return out
}

func hasProblemFor(problems []string, quoted string) bool {
	for _, p := range problems {
		if strings.HasPrefix(p, quoted+" ") {
			return true
		}
	}
	return false
}

func quote(s string) string { return `"` + s + `"` }

// check validates one value against its schema and returns it converted.
func (s Schema) check(v any) (any, error) {
	switch s.Type {
	case SchemaTypeString:
		str, err := asString(v)
		if err != nil {
			return nil, err
		}
		if len(s.Enum) > 0 && !contains(s.Enum, str) {
			return nil, fmt.Errorf("must be one of %s, got %s", strings.Join(s.Enum, ", "), quote(str))
		}
		return str, nil
	case SchemaTypeNumber:
		return asNumber(v, false)
	case SchemaTypeInteger:
		return asNumber(v, true)
	case SchemaTypeBoolean:
		return asBool(v)
	case SchemaTypeArray:
		items, ok := asSlice(v)
		if !ok {
			return nil, fmt.Errorf("must be a list, got %s", kind(v))
		}
		if s.Items == nil {
			return items, nil
		}
		out := make([]any, len(items))
		for i, it := range items {
			cv, err := s.Items.check(it)
			if err != nil {
				return nil, fmt.Errorf("item %d %s", i+1, err.Error())
			}
			out[i] = cv
		}
		return out, nil
	case SchemaTypeObject:
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("must be an object, got %s", kind(v))
		}
		if len(s.Properties) == 0 && len(s.Required) == 0 {
			return m, nil
		}
		var nested []string
		out := s.checkObject("", m, &nested)
		if len(nested) > 0 {
			return nil, fmt.Errorf("has a problem: %s", strings.Join(nested, "; "))
		}
		return out, nil
	}
	return v, nil // a type we don't know: leave it alone
}

func asString(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case bool:
		return strconv.FormatBool(x), nil
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), nil
	case int:
		return strconv.Itoa(x), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case json.Number:
		return x.String(), nil
	}
	return "", fmt.Errorf("must be text, got %s", kind(v))
}

func asNumber(v any, whole bool) (any, error) {
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case float32:
		f = float64(x)
	case int:
		f = float64(x)
	case int64:
		f = float64(x)
	case json.Number:
		var err error
		if f, err = x.Float64(); err != nil {
			return nil, fmt.Errorf("must be a number, got %s", quote(x.String()))
		}
	case string:
		var err error
		if f, err = strconv.ParseFloat(strings.TrimSpace(x), 64); err != nil {
			return nil, fmt.Errorf("must be a number, got %s", quote(x))
		}
	default:
		return nil, fmt.Errorf("must be a number, got %s", kind(v))
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, fmt.Errorf("must be a number")
	}
	if whole && f != math.Trunc(f) {
		return nil, fmt.Errorf("must be a whole number, got %s", strconv.FormatFloat(f, 'f', -1, 64))
	}
	return f, nil
}

func asBool(v any) (any, error) {
	switch x := v.(type) {
	case bool:
		return x, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		return nil, fmt.Errorf("must be true or false, got %s", quote(x))
	}
	return nil, fmt.Errorf("must be true or false, got %s", kind(v))
}

func asSlice(v any) ([]any, bool) {
	if s, ok := v.([]any); ok {
		return s, true
	}
	rv := reflect.ValueOf(v)
	if rv.IsValid() && rv.Kind() == reflect.Slice {
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = rv.Index(i).Interface()
		}
		return out, true
	}
	return nil, false
}

func kind(v any) string {
	switch v.(type) {
	case nil:
		return "nothing"
	case string:
		return "text"
	case bool:
		return "true/false"
	case float64, float32, int, int64, json.Number:
		return "a number"
	case map[string]any:
		return "an object"
	}
	if _, ok := asSlice(v); ok {
		return "a list"
	}
	return fmt.Sprintf("%T", v)
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

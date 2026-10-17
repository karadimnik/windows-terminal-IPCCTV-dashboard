package status

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Violation is one place where a document breaks the schema. It names the
// location and the keyword, never the offending value: documents carry camera
// names, and a conformance run against a live file must not print them.
type Violation struct {
	Path    string // for example $.Cameras[0].InputMode
	Keyword string // type, enum, const, required, pattern, format, minimum, oneOf, schema
	Message string
}

func (v Violation) String() string { return v.Path + ": " + v.Keyword + ": " + v.Message }

// Validate checks doc against the embedded schema.
func Validate(doc []byte) ([]Violation, error) { return ValidateAgainst(Schema, doc) }

// ValidateAgainst checks doc against schema with a validator for the subset of
// JSON Schema the status schema uses: type, enum, const, required, properties,
// items, minimum, pattern, format (date-time only), $ref (local #/$defs/...),
// allOf, oneOf, if/then. A keyword outside that subset is reported as a
// violation of keyword "schema" instead of being silently ignored, so the
// schema cannot grow past what is enforced.
func ValidateAgainst(schema, doc []byte) ([]Violation, error) {
	var root any
	if err := decodeNumbers(schema, &root); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	var instance any
	if err := decodeNumbers(doc, &instance); err != nil {
		return []Violation{{Path: "$", Keyword: "parse", Message: "not valid JSON"}}, nil
	}
	s, ok := root.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("schema: root is not an object")
	}
	v := &validator{root: s}
	v.check(s, instance, "$")
	return v.out, nil
}

func decodeNumbers(data []byte, into *any) error {
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})))
	dec.UseNumber()
	if err := dec.Decode(into); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("trailing data")
	}
	return nil
}

type validator struct {
	root map[string]any
	out  []Violation
}

var ignoredKeywords = map[string]bool{"$schema": true, "$id": true, "title": true, "description": true, "$defs": true}

func (v *validator) add(path, keyword, msg string) {
	v.out = append(v.out, Violation{Path: path, Keyword: keyword, Message: msg})
}

// valid reports whether instance satisfies schema, discarding the violations.
func (v *validator) valid(schema map[string]any, instance any, path string) bool {
	saved := v.out
	v.out = nil
	v.check(schema, instance, path)
	ok := len(v.out) == 0
	v.out = saved
	return ok
}

func (v *validator) check(schema map[string]any, instance any, path string) {
	keys := make([]string, 0, len(schema))
	for k := range schema {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		val := schema[k]
		switch k {
		case "type":
			v.checkType(val, instance, path)
		case "enum":
			list, _ := val.([]any)
			found := false
			for _, e := range list {
				if jsonEqual(e, instance) {
					found = true
					break
				}
			}
			if !found {
				v.add(path, "enum", "value is not one of the allowed values")
			}
		case "const":
			if !jsonEqual(val, instance) {
				v.add(path, "const", "value is not the required constant")
			}
		case "required":
			obj, ok := instance.(map[string]any)
			if !ok {
				continue
			}
			names, _ := val.([]any)
			for _, n := range names {
				name, _ := n.(string)
				if _, present := obj[name]; !present {
					v.add(path+"."+name, "required", "missing")
				}
			}
		case "properties":
			obj, ok := instance.(map[string]any)
			if !ok {
				continue
			}
			props, _ := val.(map[string]any)
			names := make([]string, 0, len(props))
			for n := range props {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				if child, present := obj[n]; present {
					if sub, ok := props[n].(map[string]any); ok {
						v.check(sub, child, path+"."+n)
					}
				}
			}
		case "items":
			arr, ok := instance.([]any)
			if !ok {
				continue
			}
			if sub, ok := val.(map[string]any); ok {
				for i, el := range arr {
					v.check(sub, el, fmt.Sprintf("%s[%d]", path, i))
				}
			}
		case "minimum":
			n, ok := instance.(json.Number)
			if !ok {
				continue
			}
			f, _ := n.Float64()
			if min, _ := val.(json.Number).Float64(); f < min {
				v.add(path, "minimum", "below the minimum")
			}
		case "pattern":
			s, ok := instance.(string)
			if !ok {
				continue
			}
			re, err := regexp.Compile(val.(string))
			if err != nil || !re.MatchString(s) {
				v.add(path, "pattern", "does not match the required pattern")
			}
		case "format":
			s, ok := instance.(string)
			if !ok {
				continue
			}
			if val != "date-time" {
				v.add(path, "schema", "unsupported format "+fmt.Sprint(val))
			} else if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
				v.add(path, "format", "not a valid date-time")
			}
		case "$ref":
			ref, _ := val.(string)
			target := v.resolve(ref)
			if target == nil {
				v.add(path, "schema", "unresolvable reference "+ref)
				continue
			}
			v.check(target, instance, path)
		case "allOf":
			list, _ := val.([]any)
			for _, s := range list {
				if sub, ok := s.(map[string]any); ok {
					v.check(sub, instance, path)
				}
			}
		case "oneOf":
			list, _ := val.([]any)
			n := 0
			for _, s := range list {
				if sub, ok := s.(map[string]any); ok && v.valid(sub, instance, path) {
					n++
				}
			}
			if n != 1 {
				v.add(path, "oneOf", fmt.Sprintf("matches %d alternatives, exactly 1 required", n))
			}
		case "if":
			cond, _ := val.(map[string]any)
			then, hasThen := schema["then"].(map[string]any)
			if hasThen && v.valid(cond, instance, path) {
				v.check(then, instance, path)
			}
		case "then":
			// applied by "if"
		default:
			if !ignoredKeywords[k] {
				v.add(path, "schema", "unsupported keyword "+k)
			}
		}
	}
}

func (v *validator) resolve(ref string) map[string]any {
	const prefix = "#/$defs/"
	if !strings.HasPrefix(ref, prefix) {
		return nil
	}
	defs, _ := v.root["$defs"].(map[string]any)
	target, _ := defs[strings.TrimPrefix(ref, prefix)].(map[string]any)
	return target
}

func (v *validator) checkType(want any, instance any, path string) {
	var names []string
	switch t := want.(type) {
	case string:
		names = []string{t}
	case []any:
		for _, n := range t {
			names = append(names, fmt.Sprint(n))
		}
	}
	for _, n := range names {
		if typeMatches(n, instance) {
			return
		}
	}
	v.add(path, "type", "expected "+strings.Join(names, " or ")+", got "+jsonTypeName(instance))
}

func typeMatches(name string, instance any) bool {
	switch name {
	case "null":
		return instance == nil
	case "boolean":
		_, ok := instance.(bool)
		return ok
	case "string":
		_, ok := instance.(string)
		return ok
	case "object":
		_, ok := instance.(map[string]any)
		return ok
	case "array":
		_, ok := instance.([]any)
		return ok
	case "number":
		_, ok := instance.(json.Number)
		return ok
	case "integer":
		n, ok := instance.(json.Number)
		if !ok {
			return false
		}
		if _, err := strconv.ParseInt(n.String(), 10, 64); err == nil {
			return true
		}
		f, err := n.Float64()
		return err == nil && f == math.Trunc(f) && !math.IsInf(f, 0)
	}
	return false
}

func jsonTypeName(instance any) string {
	switch instance.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case json.Number:
		return "number"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	}
	return "unknown"
}

func jsonEqual(a, b any) bool {
	an, aok := a.(json.Number)
	bn, bok := b.(json.Number)
	if aok || bok {
		if !(aok && bok) {
			return false
		}
		af, _ := an.Float64()
		bf, _ := bn.Float64()
		return af == bf
	}
	return a == b
}

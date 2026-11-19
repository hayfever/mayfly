// Command job-schema-diff compares two versions of the queue job schema and
// reports changes that break an existing reader or producer. It accepts
// additive optional properties: a property that exists only in the new
// schema is allowed when no required list names it. It rejects removed and
// renamed properties, changes to required lists, changed types, constraints,
// enums, and nested shapes.
//
// Usage: job-schema-diff OLD NEW
//
// Exit status is 0 when the change is additive, 1 when it is breaking, and 2
// on a usage or read error.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// annotationKeys carry documentation only and never affect validation.
var annotationKeys = map[string]bool{
	"$comment":    true,
	"deprecated":  true,
	"description": true,
	"examples":    true,
	"title":       true,
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: job-schema-diff OLD NEW")
		os.Exit(2)
	}
	oldDoc, err := readJSON(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "job-schema-diff: %v\n", err)
		os.Exit(2)
	}
	newDoc, err := readJSON(os.Args[2])
	if err != nil {
		fmt.Fprintf(os.Stderr, "job-schema-diff: %v\n", err)
		os.Exit(2)
	}
	problems := compare(oldDoc, newDoc)
	if len(problems) == 0 {
		return
	}
	fmt.Fprintln(os.Stderr, "job-schema-diff: the new schema is not additive:")
	for _, problem := range problems {
		fmt.Fprintf(os.Stderr, "  %s\n", problem)
	}
	os.Exit(1)
}

func readJSON(path string) (any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return doc, nil
}

// compare lists every breaking difference between two decoded schemas.
func compare(oldDoc, newDoc any) []string {
	var problems []string
	check("$", oldDoc, newDoc, &problems)
	return problems
}

func check(path string, oldVal, newVal any, problems *[]string) {
	oldMap, oldIsMap := oldVal.(map[string]any)
	newMap, newIsMap := newVal.(map[string]any)
	if !oldIsMap || !newIsMap {
		if !equalNormalized(oldVal, newVal) {
			*problems = append(*problems, fmt.Sprintf("%s changed shape", path))
		}
		return
	}

	oldRequired, oldRequiredOK := stringSet(oldMap["required"])
	newRequired, newRequiredOK := stringSet(newMap["required"])
	if oldRequiredOK || newRequiredOK {
		if !oldRequiredOK || !newRequiredOK || !sameStrings(oldRequired, newRequired) {
			*problems = append(*problems, fmt.Sprintf(
				"%s.required changed from %v to %v", path, oldMap["required"], newMap["required"]))
		}
	}

	for key, oldChild := range oldMap {
		if annotationKeys[key] || key == "required" {
			continue
		}
		newChild, exists := newMap[key]
		switch key {
		case "properties", "$defs":
			if !exists {
				*problems = append(*problems, fmt.Sprintf("%s.%s was removed", path, key))
				continue
			}
			checkProperties(path+"."+key, oldChild, newChild, problems)
		case "items":
			if !exists {
				*problems = append(*problems, fmt.Sprintf("%s.items was removed", path))
				continue
			}
			check(path+".items", oldChild, newChild, problems)
		default:
			if !exists {
				*problems = append(*problems, fmt.Sprintf("%s.%s was removed", path, key))
				continue
			}
			if !equalNormalized(oldChild, newChild) {
				*problems = append(*problems, fmt.Sprintf("%s.%s changed", path, key))
			}
		}
	}

	for key := range newMap {
		if _, exists := oldMap[key]; exists {
			continue
		}
		switch {
		case annotationKeys[key]:
			// Documentation only.
		case key == "properties", key == "$defs":
			// Additive properties and definitions are checked above; a new
			// property is optional unless a required list names it.
		default:
			*problems = append(*problems, fmt.Sprintf("%s.%s was added", path, key))
		}
	}
}

func checkProperties(path string, oldVal, newVal any, problems *[]string) {
	oldProps, oldOK := oldVal.(map[string]any)
	newProps, newOK := newVal.(map[string]any)
	if !oldOK || !newOK {
		if !equalNormalized(oldVal, newVal) {
			*problems = append(*problems, fmt.Sprintf("%s changed shape", path))
		}
		return
	}
	for name, oldProp := range oldProps {
		newProp, exists := newProps[name]
		if !exists {
			*problems = append(*problems, fmt.Sprintf("%s.%s was removed or renamed", path, name))
			continue
		}
		check(path+"."+name, oldProp, newProp, problems)
	}
	// New properties are additive. A new property that the parent requires
	// is caught by the required-list comparison in check.
}

func stringSet(value any) ([]string, bool) {
	if value == nil {
		return nil, false
	}
	items, ok := value.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, false
		}
		out = append(out, text)
	}
	return out, true
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, item := range a {
		seen[item]++
	}
	for _, item := range b {
		seen[item]--
		if seen[item] < 0 {
			return false
		}
	}
	return true
}

func equalNormalized(a, b any) bool {
	left, err := json.Marshal(normalize(a))
	if err != nil {
		return false
	}
	right, err := json.Marshal(normalize(b))
	if err != nil {
		return false
	}
	return bytes.Equal(left, right)
}

// normalize drops annotation keys so a description edit is not a break.
func normalize(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			if annotationKeys[key] {
				continue
			}
			out[key] = normalize(child)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, child := range typed {
			out[i] = normalize(child)
		}
		return out
	default:
		return value
	}
}

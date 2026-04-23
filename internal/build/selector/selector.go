// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package selector

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/opentofu/opentofu/internal/build/catalog"
)

type Match int

const (
	noMatch     Match = iota
	subtree           // module subtree might contain matches
	declaration       // matches ignoring instance keys
	exact             // full concrete match
)

func (m Match) Exact() bool       { return m >= exact }
func (m Match) Declaration() bool { return m >= declaration }
func (m Match) Possible() bool    { return m > noMatch }

type Token struct {
	Any   bool
	Value string
}

func (t Token) Match(value string) bool {
	return t.Any || t.Value == value
}

func (t Token) String() string {
	if t.Any {
		return "*"
	}
	return t.Value
}

type KeyPattern struct {
	Set   bool
	Any   bool
	Value catalog.Key
}

func (k KeyPattern) Match(value catalog.Key) bool {
	if !k.Set {
		return true
	}
	if k.Any {
		return true
	}
	return k.Value == value
}

func (k KeyPattern) String() string {
	if !k.Set {
		return ""
	}
	if k.Any {
		return "[*]"
	}
	return k.Value.String()
}

type ModuleStep struct {
	Name Token
	Key  KeyPattern
}

type Pattern struct {
	Raw       string
	Recursive bool
	Module    []ModuleStep
	Kind      catalog.TargetKind
	Type      Token
	Name      Token
	Key       KeyPattern
}

type Set []Pattern

func Parse(raw string) (Pattern, error) {
	if raw == "" {
		return Pattern{}, selectorError("a selector must not be empty")
	}
	if raw == "**" {
		return Pattern{Raw: raw, Recursive: true}, nil
	}

	pattern := Pattern{Raw: raw}
	if strings.HasSuffix(raw, ".**") {
		pattern.Recursive = true
		raw = strings.TrimSuffix(raw, ".**")
	}
	if strings.Contains(raw, "**") {
		return Pattern{}, selectorError("selector %q uses \"**\" in an unsupported position", pattern.Raw)
	}

	segments, err := splitSegments(raw)
	if err != nil {
		return Pattern{}, selectorError("%s", err)
	}

	if len(segments) == 0 {
		return Pattern{}, selectorError("selector %q is empty", pattern.Raw)
	}

	i := 0
	for i < len(segments) && segments[i] == "module" {
		if i+1 >= len(segments) {
			return Pattern{}, invalidSelectorDetail(pattern.Raw, "a module selector must include a module name")
		}
		step, parseErr := parseNamedSegment(segments[i+1], true)
		if parseErr != nil {
			return Pattern{}, invalidSelectorDetail(pattern.Raw, parseErr.Error())
		}
		pattern.Module = append(pattern.Module, ModuleStep{
			Name: step.Token,
			Key:  step.Key,
		})
		i += 2
	}

	if i == len(segments) {
		if !pattern.Recursive {
			pattern.Kind = catalog.TargetKindModule
		}
		return pattern, nil
	}

	switch segments[i] {
	case "output":
		if len(segments) != i+2 {
			return Pattern{}, invalidSelectorDetail(pattern.Raw, "an output selector must be output.<name>")
		}
		name, parseErr := parseNamedSegment(segments[i+1], false)
		if parseErr != nil {
			return Pattern{}, invalidSelectorDetail(pattern.Raw, parseErr.Error())
		}
		pattern.Kind = catalog.TargetKindOutput
		pattern.Name = name.Token
		return pattern, nil
	case "run":
		if len(segments) != i+2 {
			return Pattern{}, invalidSelectorDetail(pattern.Raw, "a run selector must be run.<name>")
		}
		name, parseErr := parseNamedSegment(segments[i+1], false)
		if parseErr != nil {
			return Pattern{}, invalidSelectorDetail(pattern.Raw, parseErr.Error())
		}
		pattern.Kind = catalog.TargetKindRun
		pattern.Name = name.Token
		return pattern, nil
	case "data":
		if len(segments) != i+3 {
			return Pattern{}, invalidSelectorDetail(pattern.Raw, "a data selector must be data.<type>.<name> or data.<type>.<name>[<key>]")
		}
		typeToken, parseErr := parseTokenSegment(segments[i+1])
		if parseErr != nil {
			return Pattern{}, invalidSelectorDetail(pattern.Raw, parseErr.Error())
		}
		name, parseErr := parseNamedSegment(segments[i+2], true)
		if parseErr != nil {
			return Pattern{}, invalidSelectorDetail(pattern.Raw, parseErr.Error())
		}
		pattern.Kind = catalog.TargetKindData
		pattern.Type = typeToken
		pattern.Name = name.Token
		pattern.Key = name.Key
		return pattern, nil
	default:
		if len(segments) != i+2 {
			return Pattern{}, invalidSelectorDetail(pattern.Raw, "a resource selector must be <type>.<name> or <type>.<name>[<key>]")
		}
		typeToken, parseErr := parseTokenSegment(segments[i])
		if parseErr != nil {
			return Pattern{}, invalidSelectorDetail(pattern.Raw, parseErr.Error())
		}
		name, parseErr := parseNamedSegment(segments[i+1], true)
		if parseErr != nil {
			return Pattern{}, invalidSelectorDetail(pattern.Raw, parseErr.Error())
		}
		pattern.Kind = catalog.TargetKindResource
		pattern.Type = typeToken
		pattern.Name = name.Token
		pattern.Key = name.Key
		return pattern, nil
	}
}

func ParseAll(raw []string) (Set, error) {
	var (
		ret  Set
		errs []error
	)

	for _, selector := range raw {
		pattern, err := Parse(selector)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		ret = append(ret, pattern)
	}

	return ret, errors.Join(errs...)
}

func (s Set) Match(addr catalog.Addr) Match {
	best := noMatch
	for _, p := range s {
		if m := p.Match(addr); m > best {
			best = m
			if best == exact {
				return exact
			}
		}
	}
	return best
}

func (p Pattern) Match(addr catalog.Addr) Match {
	if p.Raw == "**" {
		return exact
	}

	declModule := matchModule(p.Module, addr.Module, p.Recursive)
	if !declModule {
		if addr.Kind == catalog.TargetKindModule && p.mayContainMatch(addr.Module) {
			return subtree
		}
		return noMatch
	}

	exactModule := declModule && matchModuleKeys(p.Module, addr.Module)

	if p.Recursive && p.Kind == "" {
		if exactModule {
			return exact
		}
		return declaration
	}
	if p.Kind != addr.Kind {
		if addr.Kind == catalog.TargetKindModule {
			return subtree
		}
		return noMatch
	}

	switch addr.Kind {
	case catalog.TargetKindModule:
		if addr.Module.Len() != len(p.Module) {
			return noMatch
		}
		if exactModule {
			return exact
		}
		return declaration
	case catalog.TargetKindOutput, catalog.TargetKindRun:
		if !p.Name.Match(addr.Name) {
			return noMatch
		}
		if exactModule {
			return exact
		}
		return declaration
	case catalog.TargetKindResource, catalog.TargetKindData:
		if !p.Type.Match(addr.Type) || !p.Name.Match(addr.Name) {
			return noMatch
		}
		if exactModule && p.Key.Match(addr.Key) {
			return exact
		}
		return declaration
	default:
		return noMatch
	}
}

func (p Pattern) mayContainMatch(module catalog.ModulePath) bool {
	if module.Len() <= len(p.Module) {
		if module.Len() > len(p.Module) {
			return false
		}
		for i := 0; i < module.Len(); i++ {
			if !p.Module[i].Name.Match(module.Step(i).Name) {
				return false
			}
		}
		return true
	}
	return p.Recursive && p.Kind == "" && matchModule(p.Module, module, true)
}

func matchModule(pattern []ModuleStep, module catalog.ModulePath, recursive bool) bool {
	if recursive {
		if module.Len() < len(pattern) {
			return false
		}
	} else if module.Len() != len(pattern) {
		return false
	}

	for i, step := range pattern {
		if !step.Name.Match(module.Step(i).Name) {
			return false
		}
	}
	return true
}

func matchModuleKeys(pattern []ModuleStep, module catalog.ModulePath) bool {
	for i, step := range pattern {
		if !step.Key.Match(module.Step(i).Key) {
			return false
		}
	}
	return true
}

func selectorError(format string, args ...any) error {
	return fmt.Errorf("invalid build selector: "+format, args...)
}

func invalidSelectorDetail(raw, detail string) error {
	return fmt.Errorf("invalid build selector %s: %s", raw, detail)
}

type namedSegment struct {
	Token Token
	Key   KeyPattern
}

func parseNamedSegment(raw string, allowKey bool) (namedSegment, error) {
	if !allowKey {
		token, err := parseTokenSegment(raw)
		if err != nil {
			return namedSegment{}, err
		}
		return namedSegment{Token: token}, nil
	}

	nameRaw, keyRaw, hasKey, err := splitKeySuffix(raw)
	if err != nil {
		return namedSegment{}, err
	}

	token, err := parseTokenSegment(nameRaw)
	if err != nil {
		return namedSegment{}, err
	}

	ret := namedSegment{Token: token}
	if !hasKey {
		return ret, nil
	}

	key, err := parseKeyPattern(keyRaw)
	if err != nil {
		return namedSegment{}, err
	}
	ret.Key = key
	return ret, nil
}

func parseTokenSegment(raw string) (Token, error) {
	switch raw {
	case "":
		return Token{}, fmt.Errorf("missing selector segment")
	case "*":
		return Token{Any: true}, nil
	default:
		if !isIdentifier(raw) {
			return Token{}, fmt.Errorf("invalid selector segment %q", raw)
		}
		return Token{Value: raw}, nil
	}
}

func splitKeySuffix(raw string) (string, string, bool, error) {
	if raw == "" {
		return "", "", false, nil
	}

	open := strings.IndexByte(raw, '[')
	if open == -1 {
		return raw, "", false, nil
	}
	if !strings.HasSuffix(raw, "]") {
		return "", "", false, fmt.Errorf("invalid key suffix in %q", raw)
	}

	return raw[:open], raw[open+1 : len(raw)-1], true, nil
}

func parseKeyPattern(raw string) (KeyPattern, error) {
	if raw == "*" {
		return KeyPattern{Set: true, Any: true}, nil
	}

	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		value, err := strconv.Unquote(raw)
		if err != nil {
			return KeyPattern{}, fmt.Errorf("invalid string key %q", raw)
		}
		return KeyPattern{Set: true, Value: catalog.StringKey(value)}, nil
	}

	value, err := strconv.Atoi(raw)
	if err != nil {
		return KeyPattern{}, fmt.Errorf("invalid key %q", raw)
	}
	return KeyPattern{Set: true, Value: catalog.IntKey(value)}, nil
}

func splitSegments(raw string) ([]string, error) {
	var (
		segments []string
		start    int
		brackets int
		inQuote  bool
		escaped  bool
	)

	for i := 0; i < len(raw); i++ {
		ch := raw[i]

		switch {
		case escaped:
			escaped = false
		case ch == '\\':
			escaped = true
		case ch == '"':
			inQuote = !inQuote
		case !inQuote && ch == '[':
			brackets++
		case !inQuote && ch == ']':
			brackets--
			if brackets < 0 {
				return nil, fmt.Errorf("unbalanced selector brackets in %q", raw)
			}
		case !inQuote && brackets == 0 && ch == '.':
			segments = append(segments, raw[start:i])
			start = i + 1
		}
	}

	if inQuote || brackets != 0 {
		return nil, fmt.Errorf("unterminated selector segment in %q", raw)
	}

	segments = append(segments, raw[start:])
	for i, segment := range segments {
		if segment == "" {
			return nil, fmt.Errorf("empty selector segment in %q", raw)
		}
		segments[i] = strings.TrimSpace(segment)
	}
	return segments, nil
}

func isIdentifier(raw string) bool {
	if raw == "" {
		return false
	}

	for i, r := range raw {
		switch {
		case i == 0 && (unicode.IsLetter(r) || r == '_'):
		case i > 0 && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-'):
		default:
			return false
		}
	}
	return true
}

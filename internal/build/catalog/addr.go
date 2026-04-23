// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package catalog

import (
	"strconv"
	"strings"
)

type KeyKind uint8

const (
	KeyKindNone KeyKind = iota
	KeyKindInt
	KeyKindString
)

type Key struct {
	Kind KeyKind
	Int  int
	Str  string
}

func NoKey() Key {
	return Key{}
}

func IntKey(v int) Key {
	return Key{
		Kind: KeyKindInt,
		Int:  v,
	}
}

func StringKey(v string) Key {
	return Key{
		Kind: KeyKindString,
		Str:  v,
	}
}

func (k Key) String() string {
	switch k.Kind {
	case KeyKindInt:
		return "[" + strconv.Itoa(k.Int) + "]"
	case KeyKindString:
		return "[" + strconv.Quote(k.Str) + "]"
	default:
		return ""
	}
}

type ModuleStep struct {
	Name string
	Key  Key
}

type ModulePathKey string

func (k ModulePathKey) String() string {
	return string(k)
}

type ModulePath struct {
	key   ModulePathKey
	steps []ModuleStep
}

func RootModule() ModulePath {
	return ModulePath{}
}

func (m ModulePath) Child(name string, key Key) ModulePath {
	ret := make([]ModuleStep, len(m.steps)+1)
	copy(ret, m.steps)
	ret[len(m.steps)] = ModuleStep{
		Name: name,
		Key:  key,
	}

	var builder strings.Builder
	suffix := "module." + name + key.String()
	builder.Grow(len(m.key) + len(suffix) + 1)
	if m.key != "" {
		builder.WriteString(m.key.String())
		builder.WriteByte('.')
	}
	builder.WriteString(suffix)

	return ModulePath{
		key:   ModulePathKey(builder.String()),
		steps: ret,
	}
}

func (m ModulePath) Identity() ModulePathKey {
	return m.key
}

func (m ModulePath) Declaration() ModulePath {
	if len(m.steps) == 0 {
		return RootModule()
	}

	ret := RootModule()
	for _, step := range m.steps {
		ret = ret.Child(step.Name, NoKey())
	}
	return ret
}

func (m ModulePath) Len() int {
	return len(m.steps)
}

func (m ModulePath) Parent() ModulePath {
	if len(m.steps) == 0 {
		return RootModule()
	}

	ret := RootModule()
	for _, step := range m.steps[:len(m.steps)-1] {
		ret = ret.Child(step.Name, step.Key)
	}
	return ret
}

func (m ModulePath) LastStep() (ModuleStep, bool) {
	if len(m.steps) == 0 {
		return ModuleStep{}, false
	}
	return m.steps[len(m.steps)-1], true
}

func (m ModulePath) Step(i int) ModuleStep {
	return m.steps[i]
}

func (m ModulePath) String() string {
	return m.key.String()
}

type AddrKey struct {
	Module ModulePathKey
	Kind   TargetKind
	Type   string
	Name   string
	Key    Key
}

type Addr struct {
	Module ModulePath
	Kind   TargetKind
	Type   string
	Name   string
	Key    Key
}

func ModuleAddr(module ModulePath) Addr {
	return Addr{
		Module: module,
		Kind:   TargetKindModule,
	}
}

func ResourceAddr(module ModulePath, kind TargetKind, typeName, name string, key Key) Addr {
	return Addr{
		Module: module,
		Kind:   kind,
		Type:   typeName,
		Name:   name,
		Key:    key,
	}
}

func OutputAddr(module ModulePath, name string) Addr {
	return Addr{
		Module: module,
		Kind:   TargetKindOutput,
		Name:   name,
	}
}

func RunAddr(module ModulePath, name string, key Key) Addr {
	return Addr{
		Module: module,
		Kind:   TargetKindRun,
		Name:   name,
		Key:    key,
	}
}

func (a Addr) Actionable() bool {
	return a.Kind == TargetKindResource || a.Kind == TargetKindData || a.Kind == TargetKindRun
}

func (a Addr) Identity() AddrKey {
	return AddrKey{
		Module: a.Module.Identity(),
		Kind:   a.Kind,
		Type:   a.Type,
		Name:   a.Name,
		Key:    a.Key,
	}
}

func (a Addr) String() string {
	prefix := ""
	if a.Module.Len() != 0 {
		prefix = a.Module.String() + "."
	}

	switch a.Kind {
	case TargetKindModule:
		if a.Module.Len() == 0 {
			return TargetKindModule.String()
		}
		return a.Module.String()
	case TargetKindResource:
		return prefix + a.Type + "." + a.Name + a.Key.String()
	case TargetKindData:
		return prefix + "data." + a.Type + "." + a.Name + a.Key.String()
	case TargetKindRun:
		return prefix + "run." + a.Name + a.Key.String()
	case TargetKindOutput:
		return prefix + "output." + a.Name
	default:
		return prefix + "<invalid>"
	}
}

type ProviderKey struct {
	Module    ModulePathKey
	LocalName string
	Alias     string
}

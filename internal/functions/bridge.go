package functions

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/biralo-studio/biralobase/core"
	"github.com/biralo-studio/biralobase/tools/router"
)

type scope struct {
	refs map[string]reflect.Value
	next map[string]bool
}

func newScope() *scope { return &scope{refs: map[string]reflect.Value{}, next: map[string]bool{}} }
func functionError(e *remoteError) error {
	if e.Status >= 400 && e.Status <= 599 {
		return router.NewApiError(e.Status, e.Message, nil)
	}
	return errors.New(e.Message)
}

func (s *scope) export(value any) any {
	if value == nil {
		return nil
	}
	if app, ok := value.(core.App); ok {
		if _, wrapped := value.(*Admin); !wrapped {
			value = &Admin{App: app}
		}
	}
	if raw, ok := value.(json.RawMessage); ok {
		var v any
		_ = json.Unmarshal(raw, &v)
		return s.export(v)
	}
	v := reflect.ValueOf(value)
	for v.Kind() == reflect.Interface {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return nil
		}
		if len(s.refs) >= 10000 {
			return nil
		}
		id := strconv.Itoa(len(s.refs) + 1)
		s.refs[id] = v
		methods, fields := []string{}, []string{}
		for i := 0; i < v.Type().NumMethod(); i++ {
			methods = append(methods, v.Type().Method(i).Name)
		}
		if v.Elem().Kind() == reflect.Struct {
			for _, f := range reflect.VisibleFields(v.Elem().Type()) {
				if f.IsExported() {
					fields = append(fields, f.Name)
				}
			}
		}
		return map[string]any{"$ref": id, "methods": methods, "fields": fields, "type": v.Type().String()}
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return value
		}
		result := make([]any, v.Len())
		for i := range result {
			result[i] = s.export(v.Index(i).Interface())
		}
		return result
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return value
		}
		result := map[string]any{}
		iter := v.MapRange()
		for iter.Next() {
			result[iter.Key().String()] = s.export(iter.Value().Interface())
		}
		return map[string]any{"$map": result}
	default:
		if v.Kind() == reflect.Struct {
			raw, err := json.Marshal(value)
			if err == nil {
				var plain any
				if json.Unmarshal(raw, &plain) == nil {
					return s.export(plain)
				}
			}
		}
		return value
	}
}

func (s *scope) argument(raw json.RawMessage, target reflect.Type) (reflect.Value, error) {
	var ref struct {
		Ref string `json:"$ref"`
	}
	if json.Unmarshal(raw, &ref) == nil && ref.Ref != "" {
		v, ok := s.refs[ref.Ref]
		if !ok || !v.Type().AssignableTo(target) {
			return reflect.Value{}, errors.New("invalid bridge reference type")
		}
		return v, nil
	}
	if target.Kind() == reflect.Func {
		return reflect.Value{}, errors.New("Go callback arguments cannot cross the bridge")
	}
	plain, err := s.decodeWire(raw)
	if err != nil {
		return reflect.Value{}, err
	}
	if target == reflect.TypeFor[any]() {
		if plain == nil {
			return reflect.Zero(target), nil
		}
		return reflect.ValueOf(plain), nil
	}
	raw, err = json.Marshal(plain)
	if err != nil {
		return reflect.Value{}, err
	}
	v := reflect.New(target)
	if err := json.Unmarshal(raw, v.Interface()); err != nil {
		return reflect.Value{}, fmt.Errorf("argument must be %s", target)
	}
	return v.Elem(), nil
}

// Maps are explicitly tagged so user JSON containing "$ref" cannot impersonate
// a live Go object reference. Only the bridge constructs reference envelopes.
func (s *scope) decodeWire(raw json.RawMessage) (any, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	var decode func(any) (any, error)
	decode = func(value any) (any, error) {
		switch v := value.(type) {
		case []any:
			for i, item := range v {
				decoded, err := decode(item)
				if err != nil {
					return nil, err
				}
				v[i] = decoded
			}
			return v, nil
		case map[string]any:
			if id, ok := v["$ref"].(string); ok {
				ref, ok := s.refs[id]
				if !ok {
					return nil, errors.New("expired reference")
				}
				return ref.Interface(), nil
			}
			entries, ok := v["$map"].(map[string]any)
			if !ok {
				return nil, errors.New("invalid map envelope")
			}
			for key, item := range entries {
				decoded, err := decode(item)
				if err != nil {
					return nil, err
				}
				entries[key] = decoded
			}
			return entries, nil
		default:
			return value, nil
		}
	}
	return decode(value)
}

func (s *scope) call(m message) (result any, err error) {
	defer func() {
		if recover() != nil {
			result = nil
			err = errors.New("invalid Go bridge operation")
		}
	}()
	v, ok := s.refs[m.Ref]
	if !ok {
		return nil, errors.New("expired Go reference")
	}
	if m.Op == "get" || m.Op == "set" {
		if v.Kind() != reflect.Pointer || v.Elem().Kind() != reflect.Struct {
			return nil, errors.New("reference has no fields")
		}
		v = v.Elem().FieldByName(m.Name)
		if !v.IsValid() || !v.CanInterface() {
			return nil, errors.New("unknown or private field")
		}
		if m.Op == "get" {
			return v.Interface(), nil
		}
		if !v.CanSet() || len(m.Args) != 1 {
			return nil, errors.New("field is not writable")
		}
		arg, err := s.argument(m.Args[0], v.Type())
		if err != nil {
			return nil, err
		}
		v.Set(arg)
		return nil, nil
	}
	if m.Op != "method" {
		return nil, errors.New("unknown bridge operation")
	}
	fn := v.MethodByName(m.Name)
	if !fn.IsValid() {
		return nil, errors.New("unknown Go method")
	}
	ft := fn.Type()
	if len(m.Args) > 64 || (!ft.IsVariadic() && len(m.Args) != ft.NumIn()) || (ft.IsVariadic() && len(m.Args) < ft.NumIn()-1) {
		return nil, errors.New("wrong number of method arguments")
	}
	if m.Name == "Next" {
		if s.next[m.Ref] {
			return nil, errors.New("next must only be called once")
		}
		s.next[m.Ref] = true
	}
	args := make([]reflect.Value, len(m.Args))
	for i, raw := range m.Args {
		var target reflect.Type
		if ft.IsVariadic() && i >= ft.NumIn()-1 {
			target = ft.In(ft.NumIn() - 1).Elem()
		} else {
			target = ft.In(i)
		}
		args[i], err = s.argument(raw, target)
		if err != nil {
			return nil, err
		}
	}
	returned := fn.Call(args)
	values := []any{}
	errorType := reflect.TypeFor[error]()
	for _, value := range returned {
		if value.Type().Implements(errorType) {
			if !value.IsNil() {
				return nil, value.Interface().(error)
			}
		} else {
			values = append(values, value.Interface())
		}
	}
	if len(values) == 0 {
		return nil, nil
	}
	if len(values) == 1 {
		return values[0], nil
	}
	return values, nil
}

func hookName(name string) string {
	if strings.HasPrefix(name, "on") {
		return "On" + strings.TrimPrefix(name, "on")
	}
	return name
}

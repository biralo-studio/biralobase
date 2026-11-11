package functions

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/biralo-studio/biralobase/core"
)

// HookInventory is derived from the actual upstream App interface. New upstream
// hooks are available without maintaining a second hand-written registry.
func HookInventory() []string {
	t := reflect.TypeFor[core.App]()
	result := []string{}
	for i := 0; i < t.NumMethod(); i++ {
		m := t.Method(i)
		if strings.HasPrefix(m.Name, "On") && m.Type.NumOut() == 1 && strings.Contains(m.Type.Out(0).String(), "hook.") {
			result = append(result, m.Name)
		}
	}
	sort.Strings(result)
	return result
}

func (w *Worker) BindHooks(app core.App) error {
	known := map[string]bool{}
	for _, name := range HookInventory() {
		known[name] = true
	}
	for i, config := range w.Definition.Hooks {
		name := hookName(config.Event)
		if !known[name] {
			return fmt.Errorf("unknown PocketBase hook %q", config.Event)
		}
		method := reflect.ValueOf(app).MethodByName(name)
		var args []reflect.Value
		if method.Type().IsVariadic() {
			for _, tag := range config.Tags {
				args = append(args, reflect.ValueOf(tag))
			}
		} else if len(config.Tags) != 0 {
			return errors.New("this hook does not accept collection tags")
		}
		h := method.Call(args)[0]
		bind := h.MethodByName("Bind")
		handler := reflect.New(bind.Type().In(0).Elem())
		handler.Elem().FieldByName("Id").SetString("biralobase:function:" + strconv.Itoa(i))
		handler.Elem().FieldByName("Priority").SetInt(int64(config.Priority))
		callback := handler.Elem().FieldByName("Func")
		callbackType := callback.Type()
		callback.Set(reflect.MakeFunc(callbackType, func(args []reflect.Value) []reflect.Value {
			event := args[0].Interface()
			ctx := context.Background()
			_, err := w.Invoke(ctx, "hook", strconv.Itoa(i), event)
			result := reflect.Zero(callbackType.Out(0))
			if err != nil {
				result = reflect.ValueOf(err)
			}
			return []reflect.Value{result}
		}))
		bind.Call([]reflect.Value{handler})
	}
	return nil
}

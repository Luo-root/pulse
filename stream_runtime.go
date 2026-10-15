package pulse

import "reflect"

func isLiveStreamType(t reflect.Type) bool {
	return t != nil && t.Kind() == reflect.Chan
}
